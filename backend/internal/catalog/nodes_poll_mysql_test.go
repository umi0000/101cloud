package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"101cloud/backend/internal/platform"
)

func TestMySQLNodeInventoryPolling(t *testing.T) {
	ctx, db := openCatalogMySQL(t, "ci_node_inventory_")
	var mode atomic.Int32
	entered, release := make(chan struct{}, 1), make(chan struct{})
	agent := newImageTestAgentTLS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			t.Errorf("inventory attempted mutation: %s", r.Method)
		}
		switch r.URL.Path {
		case "/agent/v1/health":
			if mode.Load() == 1 {
				w.WriteHeader(503)
				w.Write([]byte(`{"status":"degraded","incus":false}`))
				return
			}
			w.Write([]byte(`{"status":"ok","incus":true}`))
		case "/agent/v1/resources":
			switch mode.Load() {
			case 2:
				w.WriteHeader(503)
				w.Write([]byte(`{}`))
				return
			case 3:
				w.Write([]byte(`{"server_resources":{}}`))
				return
			case 4:
				entered <- struct{}{}
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			}
			w.Write([]byte(`{"server_resources":{"cpu":{"total":4}},"gpus":[]}`))
		default:
			t.Errorf("unexpected inventory path: %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	a := platform.New(db, platform.Config{MasterKey: make([]byte, 32), NodeAllowCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}})
	for _, sql := range []string{
		"INSERT INTO users(id,username,password_hash,role,status) VALUES(1,'inventory-admin','unused','admin','active')",
		"INSERT INTO policy_versions(id,kind,code,version,config,status,effective_at,created_by_user_id) VALUES(1,'metrics','inventory','v1',JSON_OBJECT(),'active','2000-01-01',1)",
	} {
		if err := db.Exec(sql).Error; err != nil {
			t.Fatal(err)
		}
	}
	token, err := a.Encrypt([]byte(strings.Repeat("A", 43)))
	if err != nil {
		t.Fatal(err)
	}
	pin := sha256.Sum256(agent.Certificate().Raw)
	states := []string{"active", "draining", "maintenance", "disabled"}
	for i, state := range states {
		if i > 0 {
			agent = newImageTestAgentTLS(t, agent.Config.Handler)
			pin = sha256.Sum256(agent.Certificate().Raw)
		}
		n := platform.Row{"id": i + 1, "name": state, "metrics_policy_id": 1, "agent_endpoint": agent.URL, "token_ciphertext": token, "key_version": 1, "server_cert_sha256": pin[:], "default_storage_pool": "default", "default_project_name": "default", "allocatable_vcpus": 4, "allocatable_memory_bytes": 1, "allocatable_disk_bytes": 1, "resident_vcpu_budget": 4, "resident_memory_budget_bytes": 1, "resident_disk_budget_bytes": 1, "status": "active", "administrative_state": state, "health": "offline"}
		if err := db.Table("nodes").Create(&n).Error; err != nil {
			t.Fatal(err)
		}
	}
	get := func(id uint64) platform.Row {
		t.Helper()
		n, err := platform.Get(db, "nodes", id, false)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	// No instances and no GPUs: all registered nodes must still be observed.
	if err := pollNodeInventory(ctx, a); err != nil {
		t.Fatal(err)
	}
	for i, state := range states {
		n := get(uint64(i + 1))
		if platform.Text(n["health"]) != "online" || platform.Text(n["administrative_state"]) != state || platform.Stamp(n["resources_sampled_at"]).IsZero() || platform.Stamp(n["connection_verified_at"]).IsZero() {
			t.Fatalf("empty node was not observed safely: %v", n)
		}
		var snapshot map[string]any
		if err := json.Unmarshal([]byte(platform.Text(n["resources"])), &snapshot); err != nil {
			t.Fatal(err)
		}
		if snapshot["server_resources"].(map[string]any)["cpu"].(map[string]any)["total"] != float64(4) {
			t.Fatal("hardware inventory not saved")
		}
	}
	first := get(1)
	if err := refreshNodeInventory(ctx, a, 1); err != nil {
		t.Fatal(err)
	}
	second := get(1)
	if !platform.Stamp(second["resources_sampled_at"]).After(platform.Stamp(first["resources_sampled_at"])) || platform.Uint(second["revision"]) != platform.Uint(first["revision"]) {
		t.Fatal("periodic refresh must advance observation without invalidating edit ETag")
	}
	for _, failedMode := range []int32{1, 2, 3} {
		mode.Store(failedMode)
		before := get(1)
		if err := refreshNodeInventory(ctx, a, 1); err == nil {
			t.Fatal("failed observation succeeded")
		}
		after := get(1)
		if !platform.Stamp(after["resources_sampled_at"]).Equal(platform.Stamp(before["resources_sampled_at"])) || platform.Text(after["resources"]) != platform.Text(before["resources"]) {
			t.Fatal("failure renewed inventory")
		}
		if failedMode == 1 && (platform.Text(after["health"]) != "offline" || !platform.Stamp(after["last_heartbeat_at"]).Equal(platform.Stamp(before["last_heartbeat_at"]))) {
			t.Fatal("failed health check renewed heartbeat or stayed online")
		}
	}
	mode.Store(0)
	if err := refreshNodeInventory(ctx, a, 1); err != nil {
		t.Fatal(err)
	}
	if platform.Text(get(1)["health"]) != "online" {
		t.Fatal("node did not recover")
	}

	for _, conflict := range []string{"connection", "newer observation"} {
		mode.Store(4)
		release = make(chan struct{})
		finished := make(chan error, 1)
		go func() { finished <- refreshNodeInventory(ctx, a, 1) }()
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		changes := platform.Row{}
		if conflict == "connection" {
			changes["key_version"] = 2
		} else {
			changes["resources_sampled_at"] = a.Time().Add(time.Second)
		}
		if err := db.Table("nodes").Where("id=1").Updates(changes).Error; err != nil {
			close(release)
			t.Fatal(err)
		}
		before := get(1)
		close(release)
		if err := <-finished; err == nil {
			t.Fatalf("accepted stale %s result", conflict)
		}
		after := get(1)
		if !platform.Stamp(after["resources_sampled_at"]).Equal(platform.Stamp(before["resources_sampled_at"])) {
			t.Fatal("late response overwrote current inventory")
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := pollNodeInventory(cancelled, a); err == nil {
		t.Fatal("cancelled polling succeeded")
	}
}
