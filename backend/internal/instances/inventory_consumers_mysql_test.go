package instances

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"101cloud/backend/internal/catalog"
	"101cloud/backend/internal/platform"
)

func TestMySQLQuoteUsesHardwareFreshnessWithoutGPULoad(t *testing.T) {
	f := newPlanConfigurationFixture(t)
	f.exec(`INSERT INTO node_gpus(node_id,uuid,model,memory_mib,max_allocations,status,last_seen_at) VALUES(1,'GPU-inventory','NVIDIA A100',81920,8,'available',NOW(6))`)
	body := f.createBody("1", "2.5")
	model := "NVIDIA A100"
	body["gpu"] = catalog.GPURequest{Count: 1, Model: &model, AccessMode: "shared", MaxHourlyPT: "2.5"}
	delete(body, "policy_versions")
	for _, tc := range []struct {
		name       string
		maxAge     int
		nodeStamp  string
		gpuStamp   string
		loadStamp  string
		admissible bool
	}{
		{"thirty_second_hardware_without_load", 10, "NOW(6)-INTERVAL 30 SECOND", "NOW(6)-INTERVAL 30 SECOND", "", true},
		{"thirty_second_hardware_with_stale_load", 10, "NOW(6)-INTERVAL 30 SECOND", "NOW(6)-INTERVAL 30 SECOND", "NOW(6)-INTERVAL 61 SECOND", true},
		{"thirty_second_hardware_with_future_load", 10, "NOW(6)-INTERVAL 30 SECOND", "NOW(6)-INTERVAL 30 SECOND", "NOW(6)+INTERVAL 1 MINUTE", true},
		{"sixty_one_second_node", 120, "NOW(6)-INTERVAL 61 SECOND", "NOW(6)", "NOW(6)", false},
		{"sixty_one_second_gpu", 120, "NOW(6)", "NOW(6)-INTERVAL 61 SECOND", "NOW(6)", false},
		{"future_node", 120, "NOW(6)+INTERVAL 1 MINUTE", "NOW(6)", "NOW(6)", false},
		{"future_gpu", 120, "NOW(6)", "NOW(6)+INTERVAL 1 MINUTE", "NOW(6)", false},
		{"missing_node", 120, "NULL", "NOW(6)", "NOW(6)", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.exec(`UPDATE policy_versions SET config=JSON_SET(config,'$.metrics_max_age_seconds',?) WHERE id=4`, tc.maxAge)
			f.exec("UPDATE nodes SET resources_sampled_at=" + tc.nodeStamp + ",resources=IF(" + tc.nodeStamp + " IS NULL,NULL,JSON_OBJECT('architecture','x86_64')) WHERE id=1")
			f.exec("UPDATE node_gpus SET last_seen_at=" + tc.gpuStamp + " WHERE node_id=1")
			f.exec("DELETE FROM node_gpu_load_snapshots WHERE node_id=1")
			if tc.loadStamp != "" {
				f.exec("INSERT INTO node_gpu_load_snapshots(node_id,sampled_at,resources) VALUES(1," + tc.loadStamp + `,'{"gpus":[{"uuid":"GPU-inventory","utilization_percent":0,"memory_utilization_percent":0}]}')`)
			}
			quote := f.call(false, http.MethodPost, "/quotes", body, 0, http.StatusOK, "")
			if platform.Bool(quote["admissible"]) != tc.admissible {
				t.Fatalf("admissible=%v want=%t: %+v", quote["admissible"], tc.admissible, quote)
			}
		})
	}
	if f.count("gpu_allocations") != 0 || f.reads.Load() != 0 {
		t.Fatal("quotation reserved GPUs or performed native admission")
	}
}

func TestMySQLAdmissionAndResizeUseHardwareFreshness(t *testing.T) {
	for _, tc := range []struct {
		name    string
		maxAge  int
		stamp   string
		allowed bool
	}{
		{"thirty_seconds_with_short_metrics_age", 10, "NOW(6)-INTERVAL 30 SECOND", true},
		{"sixty_one_seconds_with_long_metrics_age", 120, "NOW(6)-INTERVAL 61 SECOND", false},
		{"future_hardware", 120, "NOW(6)+INTERVAL 1 MINUTE", false},
		{"missing_hardware", 120, "NULL", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPlanConfigurationFixture(t)
			created := f.call(false, http.MethodPost, "/instances", f.createBody("1", "2.5"), 0, http.StatusAccepted, "")
			r := f.instance(platform.Uint(created["instance_id"]))
			f.exec(`UPDATE policy_versions SET config=JSON_SET(config,'$.metrics_max_age_seconds',?) WHERE id=4`, tc.maxAge)
			f.exec("UPDATE nodes SET resources_sampled_at=" + tc.stamp + ",resources=IF(" + tc.stamp + " IS NULL,NULL,JSON_OBJECT('architecture','x86_64')) WHERE id=1")
			_, err := resizeMaxResources(f.a, f.db, r)
			replacement := Resources{VCPUs: 3, MemoryBytes: "2147483648", DiskBytes: "1073741824"}
			resizeErr := reserveExisting(t.Context(), f.a, f.db, r, &replacement)
			if tc.allowed {
				if err != nil {
					t.Fatalf("30-second hardware rejected by resize: %v", err)
				}
				if resizeErr != nil {
					t.Fatalf("30-second hardware rejected by resize admission: %v", resizeErr)
				}
				f.call(false, http.MethodPost, "/instances", f.createBody("1", "2.5"), 0, http.StatusAccepted, "")
			} else {
				var api *platform.HTTPError
				if !errors.As(err, &api) || api.Code != "node_unavailable" {
					t.Fatalf("resize accepted invalid hardware or returned wrong error: %v", err)
				}
				if !errors.As(resizeErr, &api) || api.Code != "node_unavailable" {
					t.Fatalf("resize admission accepted invalid hardware or returned wrong error: %v", resizeErr)
				}
				f.call(false, http.MethodPost, "/instances", f.createBody("1", "2.5"), 0, http.StatusConflict, "node_unavailable")
				if f.count("instances") != 1 {
					t.Fatal("rejected admission created an instance")
				}
			}
		})
	}
}

func TestMySQLReserveGPUsSeparatesHardwareAndLoadFreshness(t *testing.T) {
	f := newPlanConfigurationFixture(t)
	created := f.call(false, http.MethodPost, "/instances", f.createBody("1", "2.5"), 0, http.StatusAccepted, "")
	r := f.instance(platform.Uint(created["instance_id"]))
	f.exec(`INSERT INTO node_gpus(node_id,uuid,model,memory_mib,max_allocations,status,last_seen_at) VALUES(1,'GPU-inventory','NVIDIA A100',81920,8,'available',NOW(6))`)
	for _, tc := range []struct {
		name      string
		maxAge    int
		nodeStamp string
		gpuStamp  string
		loadStamp string
		fixed     bool
		allowed   bool
	}{
		{"short_metrics_age_accepts_thirty_second_hardware_and_fresh_load", 10, "NOW(6)-INTERVAL 30 SECOND", "NOW(6)-INTERVAL 30 SECOND", "NOW(6)", false, true},
		{"long_metrics_age_rejects_sixty_one_second_node", 120, "NOW(6)-INTERVAL 61 SECOND", "NOW(6)", "NOW(6)", false, false},
		{"long_metrics_age_rejects_sixty_one_second_gpu", 120, "NOW(6)", "NOW(6)-INTERVAL 61 SECOND", "NOW(6)", false, false},
		{"dynamic_missing_load", 10, "NOW(6)", "NOW(6)", "", false, false},
		{"dynamic_stale_load", 10, "NOW(6)", "NOW(6)", "NOW(6)-INTERVAL 11 SECOND", false, false},
		{"dynamic_future_load", 10, "NOW(6)", "NOW(6)", "NOW(6)+INTERVAL 1 MINUTE", false, false},
		{"fixed_without_load", 10, "NOW(6)-INTERVAL 30 SECOND", "NOW(6)-INTERVAL 30 SECOND", "", true, true},
		{"fixed_stale_node_without_load", 120, "NOW(6)-INTERVAL 61 SECOND", "NOW(6)", "", true, false},
		{"fixed_stale_gpu_without_load", 120, "NOW(6)", "NOW(6)-INTERVAL 61 SECOND", "", true, false},
		{"fixed_future_node_without_load", 120, "NOW(6)+INTERVAL 1 MINUTE", "NOW(6)", "", true, false},
		{"fixed_future_gpu_without_load", 120, "NOW(6)", "NOW(6)+INTERVAL 1 MINUTE", "", true, false},
		{"fixed_missing_hardware_without_load", 120, "NULL", "NOW(6)", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.exec(`UPDATE policy_versions SET config=JSON_SET(config,'$.metrics_max_age_seconds',?,'$.sm_weight',0.5,'$.memory_weight',0.5) WHERE id=4`, tc.maxAge)
			// Deliberately retain old inline load data: dynamic scheduling must not
			// fall back to utilization fields embedded in hardware inventory.
			f.exec("UPDATE nodes SET resources_sampled_at=" + tc.nodeStamp + ",resources=IF(" + tc.nodeStamp + ` IS NULL,NULL,'{"architecture":"x86_64","gpus":[{"uuid":"GPU-inventory","utilization_percent":0,"memory_utilization_percent":0}]}') WHERE id=1`)
			f.exec("UPDATE node_gpus SET last_seen_at=" + tc.gpuStamp + " WHERE node_id=1")
			f.exec("DELETE FROM node_gpu_load_snapshots WHERE node_id=1")
			if tc.loadStamp != "" {
				f.exec("INSERT INTO node_gpu_load_snapshots(node_id,sampled_at,resources) VALUES(1," + tc.loadStamp + `,'{"gpus":[{"uuid":"GPU-inventory","utilization_percent":0,"memory_utilization_percent":0}]}')`)
			}
			in := GPURequest{Count: 1, Model: "NVIDIA A100", AccessMode: "shared", MaxHourlyPT: "2.5", MinMemoryMiB: "0"}
			if tc.fixed {
				in.FixedUUIDs = []string{"GPU-inventory"}
			}
			tx := f.db.Begin()
			if tx.Error != nil {
				t.Fatal(tx.Error)
			}
			defer tx.Rollback()
			ids, err := reserveGPUs(t.Context(), f.a, tx, r, in, nil)
			if tc.allowed {
				if err != nil || len(ids) != 1 {
					t.Fatalf("reservation rejected valid evidence: ids=%v err=%v", ids, err)
				}
			} else {
				code := "gpu_capacity_unavailable"
				if strings.HasPrefix(tc.name, "dynamic_") {
					code = "observation_incomplete"
				}
				var api *platform.HTTPError
				if !errors.As(err, &api) || api.Code != code || len(ids) != 0 {
					t.Fatalf("reservation accepted invalid evidence or returned wrong error: ids=%v err=%v", ids, err)
				}
				var count int64
				if err := tx.Table("gpu_allocations").Count(&count).Error; err != nil || count != 0 {
					t.Fatalf("rejected reservation left allocations: %d, %v", count, err)
				}
			}
		})
	}
}
