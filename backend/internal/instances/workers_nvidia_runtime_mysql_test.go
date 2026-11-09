package instances

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"101cloud/backend/internal/platform"
)

// Requests use creationCleanupDB's pinned TLS transport and real worker journals.
// Native observations derive from the actual create request, with optional
// node-side configuration loss to exercise the pre-start delivery gate.
type nvidiaCreationNative struct {
	mu               sync.Mutex
	t                *testing.T
	wantRuntime      bool
	corrupt          string
	pauseObservation bool
	created          platform.Row
	state            string
	creates          int
	starts           int
	observed         bool
}

func (n *nvidiaCreationNative) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n.mu.Lock()
	defer n.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	respond := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	if r.URL.Query().Get("project") != "default" {
		n.t.Errorf("native request escaped frozen project: %s", r.URL)
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/1.0/instances":
		n.creates++
		if err := json.NewDecoder(r.Body).Decode(&n.created); err != nil {
			n.t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		cfg := object(n.created["config"])
		if n.wantRuntime {
			if cfg["nvidia.runtime"] != "true" || cfg["nvidia.driver.capabilities"] != "compute,utility" {
				n.t.Errorf("NVIDIA preparation absent from native create request: %v", cfg)
			}
		} else if cfg["nvidia.runtime"] != nil || cfg["nvidia.driver.capabilities"] != nil {
			n.t.Errorf("unapproved NVIDIA preparation in native create request: %v", cfg)
		}
		if cfg["nvidia.visible.devices"] != nil {
			n.t.Error("native create granted all visible GPUs")
		}
		devices := object(n.created["devices"])
		if len(devices) != 2 || object(devices["root"])["type"] != "disk" || object(devices["eth0"])["type"] != "nic" || len(rows(n.created["profiles"])) != 0 {
			n.t.Errorf("runtime preparation changed native devices or profiles: %v", n.created)
		}
		n.state = "Stopped"
		respond(platform.Row{"type": "async", "operation": "/1.0/operations/create"})
	case r.Method == http.MethodPut && r.URL.Path == "/1.0/instances/test-pending/state":
		var body platform.Row
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			n.t.Error(err)
		}
		if body["action"] != "start" || !n.observed || n.corrupt != "" {
			n.t.Errorf("start before approved native configuration observation: body=%v observed=%t corrupt=%s", body, n.observed, n.corrupt)
		}
		n.starts++
		n.state = "Running"
		respond(platform.Row{"type": "async", "operation": "/1.0/operations/start"})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/1.0/operations/"):
		respond(platform.Row{"metadata": platform.Row{"status_code": 200}})
	case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/storage-pools/"):
		respond(platform.Row{"metadata": platform.Row{"space": platform.Row{"total": "1099511627776", "used": "0"}}})
	case r.Method == http.MethodGet && r.URL.Path == "/1.0/instances":
		respond(platform.Row{"metadata": []platform.Row{}})
	case r.Method == http.MethodGet && r.URL.Path == "/1.0/instances/test-pending/state":
		respond(platform.Row{"metadata": platform.Row{"status": n.state}})
	case r.Method == http.MethodGet && r.URL.Path == "/1.0/instances/test-pending":
		if n.created == nil {
			http.Error(w, "not created", http.StatusNotFound)
			return
		}
		if n.pauseObservation {
			n.pauseObservation = false
			http.Error(w, "observation temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		cfg := platform.Row{}
		for key, value := range object(n.created["config"]) {
			cfg[key] = value
		}
		cfg["volatile.uuid"] = "nvidia-created-uuid"
		cfg["volatile.base_image"] = strings.Repeat("a", 64)
		switch n.corrupt {
		case "runtime-missing":
			delete(cfg, "nvidia.runtime")
		case "runtime-disabled":
			cfg["nvidia.runtime"] = "false"
		case "capabilities-missing":
			delete(cfg, "nvidia.driver.capabilities")
		case "capabilities-incomplete":
			cfg["nvidia.driver.capabilities"] = "utility"
		case "capabilities-expanded":
			cfg["nvidia.driver.capabilities"] = "all"
		}
		n.observed = true
		respond(platform.Row{"metadata": platform.Row{"name": n.created["name"], "type": n.created["type"], "architecture": n.created["architecture"], "profiles": n.created["profiles"], "config": cfg, "expanded_config": cfg, "devices": n.created["devices"], "expanded_devices": n.created["devices"]}})
	default:
		n.t.Errorf("unexpected native request: %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusBadRequest)
	}
}

func TestMySQLNvidiaRuntimeCreationFrozenCapability(t *testing.T) {
	for _, tc := range []struct {
		name        string
		snapshot    string
		imageType   string
		wantRuntime bool
	}{
		{"gpu-ready-zero-gpu", `{"creation":{"nvidia_runtime":true}}`, "container", true},
		{"cpu-image", `{"creation":{"nvidia_runtime":false}}`, "container", false},
		{"legacy-snapshot", `{}`, "container", false},
		{"vm-contradictory-bit", `{"creation":{"nvidia_runtime":true}}`, "virtual-machine", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := &nvidiaCreationNative{t: t, wantRuntime: tc.wantRuntime}
			db, a, job, _, _ := creationCleanupDB(t, n)
			for _, statement := range []string{
				"DELETE FROM gpu_allocations",
				"DELETE FROM image_nodes",
				"DELETE FROM images",
			} {
				if err := db.Exec(statement).Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Table("instances").Where("id=15").Updates(platform.Row{"incus_config_snapshot": tc.snapshot, "image_type": tc.imageType}).Error; err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			if err := runCreate(ctx, a, job); err != nil {
				t.Fatal(err)
			}
			n.mu.Lock()
			creates, starts, observed := n.creates, n.starts, n.observed
			n.mu.Unlock()
			if creates != 1 || starts != 1 || !observed {
				t.Fatalf("native create/observe/start flow missing: creates=%d starts=%d observed=%t", creates, starts, observed)
			}
			instance, err := platform.Get(db, "instances", 15, false)
			if err != nil || platform.Text(instance["provisioning_state"]) != "ready" || !platform.Bool(object(instance["incus_config_snapshot"])["creation_delivered"]) {
				t.Fatalf("verified creation not delivered: instance=%v err=%v", instance, err)
			}
			for _, query := range []struct{ table, where string }{
				{"gpu_allocations", "1=1"},
				{"usage_sessions", "charge_kind='gpu'"},
				{"credit_reservations r JOIN usage_sessions s ON s.id=r.session_id", "s.charge_kind='gpu'"},
			} {
				var count int64
				if err := db.Table(query.table).Where(query.where).Count(&count).Error; err != nil || count != 0 {
					t.Fatalf("zero-GPU preparation acquired GPU resources: table=%s count=%d err=%v", query.table, count, err)
				}
			}
			for _, key := range []string{"create.native", "state.start", "create.verify"} {
				step, err := workerStep(ctx, a, job, key)
				if err != nil || platform.Text(step["status"]) != "succeeded" {
					t.Fatalf("creation checkpoint %s missing: %v %v", key, step, err)
				}
			}
		})
	}
}

func TestMySQLNvidiaRuntimeCreationRejectsUnpreparedObservation(t *testing.T) {
	for _, corrupt := range []string{"runtime-missing", "runtime-disabled", "capabilities-missing", "capabilities-incomplete", "capabilities-expanded"} {
		t.Run(corrupt, func(t *testing.T) {
			n := &nvidiaCreationNative{t: t, wantRuntime: true, corrupt: corrupt}
			db, a, job, _, _ := creationCleanupDB(t, n)
			if err := db.Exec("DELETE FROM gpu_allocations").Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Table("instances").Where("id=15").Update("incus_config_snapshot", `{"creation":{"nvidia_runtime":true}}`).Error; err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			err := runCreate(ctx, a, job)
			if err == nil || platform.PublicError(err, true)["code"] != "creation_configuration_invalid" || !creationDefiniteError(err) {
				t.Fatalf("unprepared native result did not take confirmed configuration failure path: %v", err)
			}
			n.mu.Lock()
			creates, starts, observed := n.creates, n.starts, n.observed
			n.mu.Unlock()
			if creates != 1 || starts != 0 || !observed {
				t.Fatalf("unprepared runtime reached start: creates=%d starts=%d observed=%t", creates, starts, observed)
			}
			instance, getErr := platform.Get(db, "instances", 15, false)
			if getErr != nil || instance["creation_failed_at"] == nil || platform.Text(instance["provisioning_state"]) != "failed" || platform.Bool(object(instance["incus_config_snapshot"])["creation_delivered"]) {
				t.Fatalf("unprepared creation was not retired before delivery: instance=%v err=%v", instance, getErr)
			}
			for _, key := range []string{"state.start", "create.verify"} {
				step, stepErr := workerStep(ctx, a, job, key)
				if stepErr != nil || step != nil {
					t.Fatalf("unprepared creation reached %s checkpoint: %v %v", key, step, stepErr)
				}
			}
		})
	}
}

func TestMySQLNvidiaRuntimeCreationRecoveryIgnoresCatalogChanges(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		name := "frozen-disabled"
		if enabled {
			name = "frozen-enabled"
		}
		t.Run(name, func(t *testing.T) {
			n := &nvidiaCreationNative{t: t, wantRuntime: enabled, pauseObservation: true}
			db, a, job, _, _ := creationCleanupDB(t, n)
			if err := db.Exec("DELETE FROM gpu_allocations").Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Table("instances").Where("id=15").Update("incus_config_snapshot", jsonText(platform.Row{"creation": platform.Row{"nvidia_runtime": enabled}})).Error; err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			if err := runCreate(ctx, a, job); err == nil || creationDefiniteError(err) {
				t.Fatalf("lost post-create observation did not retain recoverable creation: %v", err)
			}
			n.mu.Lock()
			startsBeforeRecovery := n.starts
			n.mu.Unlock()
			if startsBeforeRecovery != 0 {
				t.Fatal("unobserved native creation started before recovery")
			}
			for _, table := range []string{"images", "image_nodes"} {
				if err := db.Table(table).Where("id=1").Update("gpu_ready", !enabled).Error; err != nil {
					t.Fatal(err)
				}
			}
			// Resume after the real native create succeeded but its first
			// observation failed; changed image flags cannot alter approval.
			if err := runCreate(context.WithValue(ctx, workerRecoveryKey{}, job.ID), a, job); err != nil {
				t.Fatal(err)
			}
			n.mu.Lock()
			creates, starts := n.creates, n.starts
			n.mu.Unlock()
			if creates != 1 || starts != 1 {
				t.Fatalf("recovery replayed create/start: creates=%d starts=%d", creates, starts)
			}
			step, err := workerStep(ctx, a, job, "create.native")
			if err != nil || platform.Text(step["status"]) != "succeeded" {
				t.Fatalf("recovered native creation not confirmed: %v %v", step, err)
			}
		})
	}
}

func TestMySQLNvidiaRuntimeCreationAllocatedLegacyStillPrepares(t *testing.T) {
	n := &nvidiaCreationNative{t: t, wantRuntime: true, corrupt: "runtime-missing"}
	db, a, job, _, _ := creationCleanupDB(t, n)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	// The fixture's existing reserved allocation must enable NVIDIA even though
	// its legacy snapshot has no nvidia_runtime field. Rejecting the node-side
	// loss before attach isolates this from the unchanged GPU attach workflow.
	err := runCreate(ctx, a, job)
	if err == nil || platform.PublicError(err, true)["code"] != "creation_configuration_invalid" {
		t.Fatalf("legacy allocated creation lost its NVIDIA configuration gate: %v", err)
	}
	n.mu.Lock()
	creates, starts := n.creates, n.starts
	n.mu.Unlock()
	if creates != 1 || starts != 0 {
		t.Fatalf("legacy allocation create/start counts=%d/%d", creates, starts)
	}
	allocation, err := platform.Get(db, "gpu_allocations", 1, false)
	if err != nil || platform.Text(allocation["status"]) != "reserved" || platform.Bool(allocation["configured"]) || platform.Bool(allocation["observed"]) {
		t.Fatalf("configuration rejection mutated GPU attachment: %v %v", allocation, err)
	}
}
