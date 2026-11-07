package catalog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"101cloud/backend/internal/platform"
	"github.com/gin-gonic/gin"
)

// Exercise the real registered worker cadence and HTTP consumer, rather than
// invoking a poll function directly. All databases and TLS endpoints are local.
func TestMySQLInventoryAndLoadWorkerHTTPSmoke(t *testing.T) {
	ctx, a := newGPULoadMySQLApp(t)
	var healthCalls, resourceCalls atomic.Int32
	agent := newImageTestAgentTLS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/agent/v1/health":
			healthCalls.Add(1)
			w.Write([]byte(`{"status":"ok","incus":true}`))
		case "/agent/v1/resources":
			resourceCalls.Add(1)
			w.Write([]byte(gpuLoadTestResponse))
		default:
			t.Errorf("unexpected Agent path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	addGPULoadMySQLNode(t, a, 1, agent, "active", true)
	if err := a.DB.Exec(`INSERT INTO policy_versions(id,kind,code,version,config,status,effective_at,created_by_user_id) VALUES(2,'gpu-scheduling','worker-smoke','1','{"poll_interval_seconds":2,"metrics_max_age_seconds":5}','active','2000-01-01',1)`).Error; err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	registerNodes(a, router.Group("/admin"))
	server := httptest.NewServer(router)
	defer server.Close()
	workerCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.Run(workerCtx)
	}()
	defer func() {
		cancel()
		<-done
	}()
	wait := func(label string, duration time.Duration, predicate func() bool) {
		t.Helper()
		deadline := time.Now().Add(duration)
		for !predicate() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s (health=%d resources=%d)", label, healthCalls.Load(), resourceCalls.Load())
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	wait("initial inventory and independent load", 10*time.Second, func() bool {
		var inventory, load int64
		if err := a.DB.Table("nodes").Where("id=1 AND resources_sampled_at>?", a.Time().Add(-time.Minute)).Count(&inventory).Error; err != nil {
			t.Fatal(err)
		}
		if err := a.DB.Table("node_gpu_load_snapshots").Where("node_id=1").Count(&load).Error; err != nil {
			t.Fatal(err)
		}
		return inventory == 1 && load == 1
	})
	first := gpuLoadMySQLRow(t, a, "nodes", 1)
	firstLoad := gpuLoadMySQLRow(t, a, "node_gpu_load_snapshots", 1)
	wait("policy load cadence before hardware cadence", 8*time.Second, func() bool {
		return platform.Stamp(gpuLoadMySQLRow(t, a, "node_gpu_load_snapshots", 1)["sampled_at"]).After(platform.Stamp(firstLoad["sampled_at"]))
	})
	if current := gpuLoadMySQLRow(t, a, "nodes", 1); !platform.Stamp(current["resources_sampled_at"]).Equal(platform.Stamp(first["resources_sampled_at"])) {
		t.Fatal("dynamic worker renewed hardware inventory before its 20-second poll")
	}
	wait("20-second hardware worker refresh", 25*time.Second, func() bool {
		return platform.Stamp(gpuLoadMySQLRow(t, a, "nodes", 1)["resources_sampled_at"]).After(platform.Stamp(first["resources_sampled_at"]))
	})
	response, err := server.Client().Get(server.URL + "/admin/nodes/1/resources")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body struct {
		Data struct {
			Observed string `json:"observed_at"`
			Quality  string `json:"quality"`
			Health   string `json:"health"`
		} `json:"data"`
	}
	if err = json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || body.Data.Quality != "cached" || body.Data.Health != "online" || body.Data.Observed == "" {
		t.Fatalf("HTTP inventory did not expose worker observation: status=%d body=%+v", response.StatusCode, body)
	}
	if healthCalls.Load() < 2 || resourceCalls.Load() <= healthCalls.Load() {
		t.Fatalf("independent worker cadences absent: health=%d resources=%d", healthCalls.Load(), resourceCalls.Load())
	}
	t.Logf("real worker + HTTP + pinned TLS Agent: health=%d resources=%d; inventory=%s -> %s; load=%s", healthCalls.Load(), resourceCalls.Load(), platform.Stamp(first["resources_sampled_at"]).Format(time.RFC3339Nano), body.Data.Observed, platform.Stamp(gpuLoadMySQLRow(t, a, "node_gpu_load_snapshots", 1)["sampled_at"]).Format(time.RFC3339Nano))
}
