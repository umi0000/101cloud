package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"101cloud/backend/internal/platform"
)

const gpuLoadTestUUID = "GPU-11111111-1111-1111-1111-111111111111"
const gpuLoadTestResponse = `{"server_resources":{"cpu":{"total":8}},"gpus":[{"uuid":"GPU-11111111-1111-1111-1111-111111111111","model":"Observed GPU","pci_address":"0000:01:00.0","memory_total_mib":8192,"memory_used_mib":2048,"utilization_percent":25,"memory_utilization_percent":50,"load_percent":50}],"private_diagnostic":"must not be persisted"}`

func newGPULoadMySQLApp(t *testing.T) (context.Context, *platform.App) {
	t.Helper()
	ctx, db := openCatalogMySQL(t, "ci_gpu_load_")
	for _, statement := range []string{
		"INSERT INTO users(id,username,password_hash,role,status) VALUES(1,'gpu-load-admin','unused','admin','active')",
		"INSERT INTO policy_versions(id,kind,code,version,config,status,effective_at,created_by_user_id) VALUES(1,'metrics','inventory','v1',JSON_OBJECT(),'active','2000-01-01',1)",
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	return ctx, platform.New(db, platform.Config{MasterKey: make([]byte, 32), NodeAllowCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}})
}

func addGPULoadMySQLNode(t *testing.T, a *platform.App, id uint64, agent *httptest.Server, state string, gpu bool) {
	t.Helper()
	token, err := a.Encrypt([]byte(strings.Repeat("A", 43)))
	if err != nil {
		t.Fatal(err)
	}
	pin := sha256.Sum256(agent.Certificate().Raw)
	old := a.Time().Add(-5 * time.Minute)
	n := platform.Row{"id": id, "name": fmt.Sprintf("gpu-load-%d", id), "metrics_policy_id": 1, "agent_endpoint": agent.URL, "token_ciphertext": token, "key_version": 1, "server_cert_sha256": pin[:], "default_storage_pool": "default", "default_project_name": "default", "allocatable_vcpus": 4, "allocatable_memory_bytes": 1, "allocatable_disk_bytes": 1, "resident_vcpu_budget": 4, "resident_memory_budget_bytes": 1, "resident_disk_budget_bytes": 1, "status": "active", "administrative_state": state, "health": "online", "resources": `{"server_resources":{"cpu":{"total":4}},"gpus":[]}`, "resources_sampled_at": old, "last_heartbeat_at": old, "connection_verified_at": old}
	if err := a.DB.Table("nodes").Create(&n).Error; err != nil {
		t.Fatal(err)
	}
	if gpu {
		g := platform.Row{"node_id": id, "uuid": gpuLoadTestUUID, "model": "Original GPU", "memory_mib": 4096, "max_allocations": 1, "status": "disabled", "last_seen_at": old}
		if err := a.DB.Table("node_gpus").Create(&g).Error; err != nil {
			t.Fatal(err)
		}
	}
}

func gpuLoadMySQLRow(t *testing.T, a *platform.App, table string, id uint64) platform.Row {
	t.Helper()
	var row platform.Row
	key := "node_id"
	if table == "nodes" {
		key = "id"
	}
	if err := a.DB.Table(table).Where(key+" = ?", id).Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	return row
}

func saveGPULoadMySQLSnapshot(t *testing.T, a *platform.App, id uint64, age time.Duration) {
	t.Helper()
	row := platform.Row{"node_id": id, "sampled_at": a.Time().Add(-age), "resources": `{"gpus":[]}`}
	if err := a.DB.Table("node_gpu_load_snapshots").Create(&row).Error; err != nil {
		t.Fatal(err)
	}
}

func assertGPULoadSnapshotUnchanged(t *testing.T, before, after platform.Row) {
	t.Helper()
	if !platform.Stamp(before["sampled_at"]).Equal(platform.Stamp(after["sampled_at"])) || platform.Text(before["resources"]) != platform.Text(after["resources"]) {
		t.Fatalf("dynamic snapshot changed unexpectedly: before=%v after=%v", before, after)
	}
}

func TestMySQLNodeGPULoadPolling(t *testing.T) {
	ctx, a := newGPULoadMySQLApp(t)
	var calls [7]atomic.Int32
	states := []string{"active", "active", "active", "active", "maintenance", "draining", "disabled"}
	for i, state := range states {
		agent := newImageTestAgentTLS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls[i].Add(1)
			if r.Method != http.MethodGet || r.URL.Path != "/agent/v1/resources" {
				t.Errorf("unexpected dynamic observation: %s %s", r.Method, r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(gpuLoadTestResponse))
		}))
		addGPULoadMySQLNode(t, a, uint64(i+1), agent, state, i != 3)
	}
	saveGPULoadMySQLSnapshot(t, a, 2, 45*time.Second)
	saveGPULoadMySQLSnapshot(t, a, 3, 5*time.Second)
	if err := pollNodeGPULoad(ctx, a); err != nil {
		t.Fatal(err)
	}
	for i := range calls {
		if calls[i].Load() != 0 {
			t.Fatal("polling ran without a published positive scheduling interval")
		}
	}
	// No instance or plan references any policy: the shortest published interval
	// is global, including retired policies still usable by frozen instances.
	for _, statement := range []string{
		`INSERT INTO policy_versions(id,kind,code,version,config,status,effective_at,created_by_user_id) VALUES
		(2,'gpu-scheduling','slow','1','{"poll_interval_seconds":120}','active','2000-01-01',1),
		(3,'gpu-scheduling','shortest','1','{"poll_interval_seconds":20}','retired','2000-01-01',1),
		(4,'gpu-scheduling','zero','1','{"poll_interval_seconds":0}','active','2000-01-01',1),
		(5,'gpu-scheduling','draft','1','{"poll_interval_seconds":1}','draft','2000-01-01',1),
		(6,'gpu-scheduling','future','1','{"poll_interval_seconds":1}','active','2099-01-01',1)`,
	} {
		if err := a.DB.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	fresh := gpuLoadMySQLRow(t, a, "node_gpu_load_snapshots", 3)
	if err := pollNodeGPULoad(ctx, a); err != nil {
		t.Fatal(err)
	}
	for i := range calls {
		want := int32(0)
		if i < 2 {
			want = 1
		}
		if got := calls[i].Load(); got != want {
			t.Errorf("node %d (%s) requests=%d, want %d", i+1, states[i], got, want)
		}
	}
	for _, id := range []uint64{1, 2} {
		if age := a.Time().Sub(platform.Stamp(gpuLoadMySQLRow(t, a, "node_gpu_load_snapshots", id)["sampled_at"])); age < 0 || age > 10*time.Second {
			t.Fatalf("node %d dynamic sample was not refreshed: age=%v", id, age)
		}
	}
	assertGPULoadSnapshotUnchanged(t, fresh, gpuLoadMySQLRow(t, a, "node_gpu_load_snapshots", 3))
	if err := pollNodeGPULoad(ctx, a); err != nil {
		t.Fatal(err)
	}
	if calls[0].Load() != 1 || calls[1].Load() != 1 {
		t.Fatal("fresh snapshots were polled again before the global interval")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := pollNodeGPULoad(cancelled, a); err == nil {
		t.Fatal("cancelled dynamic polling succeeded")
	}
}

func TestMySQLNodeGPULoadRefreshIsolationAndFailures(t *testing.T) {
	ctx, a := newGPULoadMySQLApp(t)
	var mode atomic.Int32
	agent := newImageTestAgentTLS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			t.Errorf("observation attempted mutation: %s", r.Method)
		}
		switch r.URL.Path {
		case "/agent/v1/health":
			w.Write([]byte(`{"status":"ok","incus":true}`))
		case "/agent/v1/resources":
			switch mode.Load() {
			case 1:
				w.WriteHeader(http.StatusServiceUnavailable)
				w.Write([]byte(`{}`))
			case 2:
				w.Write([]byte(`{"gpus":`))
			case 3:
				w.Write([]byte(`{"server_resources":{}}`))
			default:
				w.Write([]byte(gpuLoadTestResponse))
			}
		default:
			t.Errorf("unexpected observation path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	addGPULoadMySQLNode(t, a, 1, agent, "active", true)
	inventory := gpuLoadMySQLRow(t, a, "nodes", 1)
	gpu := gpuLoadMySQLRow(t, a, "node_gpus", 1)
	if err := refreshNodeGPULoad(ctx, a, 1); err != nil {
		t.Fatal(err)
	}
	afterInventory := gpuLoadMySQLRow(t, a, "nodes", 1)
	for _, field := range []string{"resources", "resources_sampled_at", "last_heartbeat_at", "connection_verified_at", "health", "revision"} {
		if platform.Text(inventory[field]) != platform.Text(afterInventory[field]) {
			t.Errorf("dynamic refresh changed node inventory field %s", field)
		}
	}
	afterGPU := gpuLoadMySQLRow(t, a, "node_gpus", 1)
	for _, field := range []string{"model", "memory_mib", "last_seen_at", "status", "revision"} {
		if platform.Text(gpu[field]) != platform.Text(afterGPU[field]) {
			t.Errorf("dynamic refresh changed GPU inventory field %s", field)
		}
	}
	snapshot := gpuLoadMySQLRow(t, a, "node_gpu_load_snapshots", 1)
	var resources map[string]any
	if err := json.Unmarshal([]byte(platform.Text(snapshot["resources"])), &resources); err != nil {
		t.Fatal(err)
	}
	gpus, ok := resources["gpus"].([]any)
	if !ok || len(gpus) != 1 || len(resources) != 1 {
		t.Fatalf("dynamic snapshot must contain only GPU load: %v", resources)
	}
	load, ok := gpus[0].(map[string]any)
	if !ok || load["uuid"] != gpuLoadTestUUID || load["utilization_percent"] != float64(25) || load["memory_utilization_percent"] != float64(50) {
		t.Fatalf("dynamic GPU measurements not saved: %v", gpus)
	}
	for key := range load {
		if key != "uuid" && key != "utilization_percent" && key != "memory_utilization_percent" && key != "error" {
			t.Errorf("inventory or private diagnostic leaked into load snapshot: %s", key)
		}
	}
	if err := refreshNodeInventory(ctx, a, 1); err != nil {
		t.Fatal(err)
	}
	if !platform.Stamp(gpuLoadMySQLRow(t, a, "nodes", 1)["resources_sampled_at"]).After(platform.Stamp(inventory["resources_sampled_at"])) || !platform.Stamp(gpuLoadMySQLRow(t, a, "node_gpus", 1)["last_seen_at"]).After(platform.Stamp(gpu["last_seen_at"])) {
		t.Fatal("inventory observation did not renew hardware timestamps")
	}
	assertGPULoadSnapshotUnchanged(t, snapshot, gpuLoadMySQLRow(t, a, "node_gpu_load_snapshots", 1))
	for _, failedMode := range []int32{1, 2, 3} {
		mode.Store(failedMode)
		if err := refreshNodeGPULoad(ctx, a, 1); err == nil {
			t.Fatalf("failed dynamic observation mode %d succeeded", failedMode)
		}
		assertGPULoadSnapshotUnchanged(t, snapshot, gpuLoadMySQLRow(t, a, "node_gpu_load_snapshots", 1))
	}
	mode.Store(0)
	if err := refreshNodeGPULoad(ctx, a, 1); err != nil {
		t.Fatal(err)
	}
	if !platform.Stamp(gpuLoadMySQLRow(t, a, "node_gpu_load_snapshots", 1)["sampled_at"]).After(platform.Stamp(snapshot["sampled_at"])) {
		t.Fatal("successful recovery did not advance dynamic observation")
	}
}

func TestMySQLNodeGPULoadRefreshTrustFenceWithoutNetworkLocks(t *testing.T) {
	ctx, a := newGPULoadMySQLApp(t)
	entered, release := make(chan struct{}, 1), make(chan struct{})
	var unblock sync.Once
	defer unblock.Do(func() { close(release) })
	agent := newImageTestAgentTLS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/agent/v1/resources" {
			t.Errorf("unexpected dynamic observation: %s %s", r.Method, r.URL.Path)
		}
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(gpuLoadTestResponse))
	}))
	addGPULoadMySQLNode(t, a, 1, agent, "active", true)
	saveGPULoadMySQLSnapshot(t, a, 1, time.Minute)
	before := gpuLoadMySQLRow(t, a, "node_gpu_load_snapshots", 1)
	finished := make(chan error, 1)
	go func() { finished <- refreshNodeGPULoad(ctx, a, 1) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// An administrator must be able to replace trust while the old Agent is
	// blocked. Lock all observation rows through another connection: this would
	// time out if refresh held a transaction across its network request.
	lockCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	tx := a.DB.WithContext(lockCtx).Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	if err := tx.Exec("SELECT id FROM nodes WHERE id=1 FOR UPDATE").Error; err != nil {
		t.Fatal(err)
	}
	if err := tx.Exec("SELECT id FROM node_gpus WHERE node_id=1 FOR UPDATE").Error; err != nil {
		t.Fatal(err)
	}
	if err := tx.Exec("SELECT node_id FROM node_gpu_load_snapshots WHERE node_id=1 FOR UPDATE").Error; err != nil {
		t.Fatal(err)
	}
	replacement := newImageTestAgentTLS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("in-flight refresh unexpectedly contacted replacement Agent")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	pin := sha256.Sum256(replacement.Certificate().Raw)
	token, err := a.Encrypt([]byte(strings.Repeat("B", 43)))
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Table("nodes").Where("id=1").Updates(platform.Row{"agent_endpoint": replacement.URL, "server_cert_sha256": pin[:], "token_ciphertext": token, "key_version": 2}).Error; err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit().Error; err != nil {
		t.Fatal(err)
	}
	unblock.Do(func() { close(release) })
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("accepted old Agent response after trust replacement")
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	assertGPULoadSnapshotUnchanged(t, before, gpuLoadMySQLRow(t, a, "node_gpu_load_snapshots", 1))
}
