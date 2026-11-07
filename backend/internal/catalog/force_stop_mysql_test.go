package catalog

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"101cloud/backend/internal/platform"
	"gorm.io/gorm"
)

func stopCatalogFixtureTask(t *testing.T, f *imageJobFixture, job platform.Job) platform.Row {
	t.Helper()
	if err := f.db.Transaction(func(tx *gorm.DB) error {
		row, err := platform.Get(tx, "tasks", job.ID, true)
		if err != nil {
			return err
		}
		if err = ForceStopTask(tx, row, f.a.Time()); err != nil {
			return err
		}
		return tx.Table("tasks").Where("id=?", job.ID).Updates(platform.Row{"status": "cancelled", "error_code": "task_force_stopped", "lease_generation": gorm.Expr("lease_generation+1"), "lease_owner": nil, "lease_until": nil, "finished_at": f.a.Time(), "result_redacted": `{"force_stop":{"orchestration_stopped":true,"requires_reconciliation":true,"external_state":"requires_reconciliation","resources_retained":true}}`, "revision": gorm.Expr("revision+1")}).Error
	}); err != nil {
		t.Fatal(err)
	}
	stopped, err := platform.Get(f.db, "tasks", job.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	return stopped
}

func awaitCatalogIO(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("native observation did not start")
	}
}

func awaitStoppedCatalogJob(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("late native result was accepted after cancellation")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled consumer did not finish")
	}
}

func TestMySQLForceStoppedImageRejectsLateReady(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	defer unblock.Do(func() { close(release) })
	f := newImageJobFixture(t, func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		imageJobNative(w, imageJobNativeImage())
	})
	job, id := f.selection("", false)
	done := make(chan error, 1)
	go func() { done <- imagePrepareJob(f.ctx, f.a, job) }()
	awaitCatalogIO(t, entered)
	stopCatalogFixtureTask(t, f, job)
	before := f.state(id, "unknown")
	unblock.Do(func() { close(release) })
	awaitStoppedCatalogJob(t, done)
	after := f.state(id, "unknown")
	if platform.Uint(before["revision"]) != platform.Uint(after["revision"]) || after["last_verified_at"] != nil {
		t.Fatal("late image observation mutated the stopped projection")
	}
}

func TestMySQLForceStoppedNodeConsumersRejectLateInventory(t *testing.T) {
	for _, kind := range []string{"node.probe", "node.drain"} {
		t.Run(kind, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var unblock sync.Once
			defer unblock.Do(func() { close(release) })
			f := newImageJobFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/agent/v1/health":
					_, _ = w.Write([]byte(`{"status":"ok","incus":true}`))
				case "/agent/v1/resources":
					_, _ = w.Write([]byte(`{"server_resources":{"cpu":{"total":4}},"gpus":[]}`))
				case "/1.0":
					_, _ = w.Write([]byte(`{"type":"sync","status_code":200,"metadata":{"environment":{"server_clustered":false}}}`))
				case "/1.0/projects":
					close(entered)
					<-release
					_, _ = w.Write([]byte(`{"type":"sync","status_code":200,"metadata":[]}`))
				default:
					t.Errorf("unexpected inventory call %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			})
			job, _ := f.selection("", false)
			job.Type, job.Parameters = kind, json.RawMessage(`{}`)
			if err := f.db.Table("tasks").Where("id=?", job.ID).Updates(platform.Row{"task_type": kind, "parameters_redacted": "{}"}).Error; err != nil {
				t.Fatal(err)
			}
			if kind == "node.drain" {
				if err := f.db.Table("nodes").Where("id=?", job.NodeID).Updates(platform.Row{"status": "draining", "administrative_state": "draining"}).Error; err != nil {
					t.Fatal(err)
				}
			}
			before, err := platform.Get(f.db, "nodes", job.NodeID, false)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- nodeInventoryJob(f.ctx, f.a, job) }()
			awaitCatalogIO(t, entered)
			stopCatalogFixtureTask(t, f, job)
			unblock.Do(func() { close(release) })
			awaitStoppedCatalogJob(t, done)
			after, err := platform.Get(f.db, "nodes", job.NodeID, false)
			if err != nil {
				t.Fatal(err)
			}
			if platform.Uint(after["revision"]) != platform.Uint(before["revision"]) || after["resources_sampled_at"] != nil || platform.Text(after["administrative_state"]) != platform.Text(before["administrative_state"]) {
				t.Fatal("stopped inventory consumer published late observations or undid drain")
			}
		})
	}
}

func TestMySQLForceStoppedNetworkRejectsLateVerification(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	defer unblock.Do(func() { close(release) })
	f := newImageJobFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/agent/v1/resources" {
			_, _ = w.Write([]byte(`{"network_interfaces":[{"name":"br0","up":true,"addresses":[{"address":"192.0.2.2"}]}]}`))
			return
		}
		close(entered)
		<-release
		_, _ = w.Write([]byte(`{"metadata":[{"name":"br0","type":"bridge","managed":false}]}`))
	})
	job, _ := f.selection("", false)
	for _, statement := range []string{
		"INSERT INTO network_pools(id,name,address_family,network_address,prefix_length,range_start,range_end,gateway,dns_servers,status,revision) VALUES(1,'stop-test',4,INET6_ATON('192.0.2.0'),24,INET6_ATON('192.0.2.100'),INET6_ATON('192.0.2.110'),INET6_ATON('192.0.2.2'),JSON_ARRAY(),'active',1)",
		"INSERT INTO node_network_bindings(id,node_id,network_pool_id,project_name,mode,parent_interface,status,validation_status,validation_details,revision) VALUES(1,1,1,'default','bridge','br0','disabled','pending',JSON_OBJECT(),1)",
	} {
		if err := f.db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	job.Type, job.Parameters = "network.validate", json.RawMessage(`{"binding_id":"1","revision":"1"}`)
	if err := f.db.Table("tasks").Where("id=?", job.ID).Updates(platform.Row{"task_type": job.Type, "parameters_redacted": string(job.Parameters)}).Error; err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- networkValidateJob(f.ctx, f.a, job) }()
	awaitCatalogIO(t, entered)
	stopCatalogFixtureTask(t, f, job)
	before, err := platform.Get(f.db, "node_network_bindings", 1, false)
	if err != nil {
		t.Fatal(err)
	}
	unblock.Do(func() { close(release) })
	awaitStoppedCatalogJob(t, done)
	after, err := platform.Get(f.db, "node_network_bindings", 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if platform.Text(after["validation_status"]) != "failed" || platform.Text(after["status"]) != "disabled" || platform.Uint(after["revision"]) != platform.Uint(before["revision"]) {
		t.Fatal("late network verification changed the stopped binding")
	}
}

func TestMySQLForceStoppedImageRecoveryNeverReplaysDownload(t *testing.T) {
	for _, exists := range []bool{false, true} {
		name := "absent"
		if exists {
			name = "present"
		}
		t.Run(name, func(t *testing.T) {
			f := newImageJobFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("cancelled image recovery attempted native mutation: %s", r.Method)
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				if exists {
					imageJobNative(w, imageJobNativeImage())
				} else {
					imageJobMissing(w)
				}
			})
			original, id := f.selection("images", false)
			source := stopCatalogFixtureTask(t, f, original)
			queued, err := platform.Enqueue(f.db, "instance.reconcile", original.UserID, original.NodeID, 0, false, platform.Row{"source_task_id": original.ID})
			if err != nil {
				t.Fatal(err)
			}
			wrapper := platform.Job{ID: platform.Uint(queued["task_id"]), Type: "instance.reconcile", UserID: original.UserID, NodeID: original.NodeID, LeaseGeneration: 1}
			if err = f.db.Table("tasks").Where("id=?", wrapper.ID).Updates(platform.Row{"status": "running", "lease_generation": 1, "lease_owner": "force-stop-reconcile-test", "lease_until": f.a.Time().Add(time.Minute)}).Error; err != nil {
				t.Fatal(err)
			}
			err = ReconcileForceStoppedTask(f.ctx, f.a, wrapper, source)
			if (err == nil) != exists {
				t.Fatalf("unexpected observation-only recovery result: %v", err)
			}
			want := "unknown"
			if exists {
				want = "ready"
			}
			f.state(id, want)
			retained, err := platform.Get(f.db, "tasks", original.ID, false)
			if err != nil {
				t.Fatal(err)
			}
			if platform.Text(retained["status"]) != "cancelled" || platform.Text(retained["error_code"]) != "task_force_stopped" || platform.Uint(retained["lease_generation"]) != platform.Uint(source["lease_generation"]) {
				t.Fatal("reconciliation rewrote cancelled source history")
			}
		})
	}
}

func TestMySQLForceStoppedImageRecoveryFencesLateObservation(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	defer unblock.Do(func() { close(release) })
	f := newImageJobFixture(t, func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		imageJobNative(w, imageJobNativeImage())
	})
	original, id := f.selection("images", false)
	source := stopCatalogFixtureTask(t, f, original)
	queued, err := platform.Enqueue(f.db, "instance.reconcile", original.UserID, original.NodeID, 0, false, platform.Row{"source_task_id": original.ID})
	if err != nil {
		t.Fatal(err)
	}
	wrapper := platform.Job{ID: platform.Uint(queued["task_id"]), Type: "instance.reconcile", UserID: original.UserID, NodeID: original.NodeID, LeaseGeneration: 1}
	if err = f.db.Table("tasks").Where("id=?", wrapper.ID).Updates(platform.Row{"status": "running", "lease_generation": 1, "lease_owner": "force-stop-reconcile-test", "lease_until": f.a.Time().Add(time.Minute)}).Error; err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- ReconcileForceStoppedTask(f.ctx, f.a, wrapper, source) }()
	awaitCatalogIO(t, entered)
	stopCatalogFixtureTask(t, f, wrapper)
	unblock.Do(func() { close(release) })
	awaitStoppedCatalogJob(t, done)
	f.state(id, "unknown")
	retained, err := platform.Get(f.db, "tasks", original.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if platform.Uint(retained["revision"]) != platform.Uint(source["revision"]) {
		t.Fatal("stopped reconciliation published completion evidence on its source")
	}
}
