package monitoring

import (
	"context"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"101cloud/backend/internal/platform"
	"github.com/gin-gonic/gin"
)

// These connection-local fixtures have only explicit bindings. In particular,
// no platform binding table exists for a worker or read to fall back to.
func explicitMonitoringApp(t *testing.T) *platform.App {
	t.Helper()
	dsn := os.Getenv("CLOUD_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set CLOUD_TEST_MYSQL_DSN for explicit monitoring regressions")
	}
	db, err := platform.OpenDB(platform.Config{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	t.Cleanup(func() { tx.Rollback() })
	definitions := map[string]string{
		"policy_versions":             "id BIGINT PRIMARY KEY,kind VARCHAR(32),config JSON NOT NULL",
		"nodes":                       "id BIGINT PRIMARY KEY,metrics_policy_id BIGINT NOT NULL",
		"instances":                   "id BIGINT PRIMARY KEY,node_id BIGINT,user_id BIGINT,policy_versions JSON NOT NULL,deleted_at DATETIME(6)",
		"node_metric_cursors":         "node_id BIGINT,project_scope VARCHAR(64),last_sampled_at DATETIME(6),PRIMARY KEY(node_id,project_scope)",
		"metric_rollup_progress":      "node_id BIGINT,bucket_seconds BIGINT,through_at DATETIME(6),PRIMARY KEY(node_id,bucket_seconds)",
		"instance_metric_samples":     "id BIGINT PRIMARY KEY,instance_id BIGINT,node_id BIGINT,sampled_at DATETIME(6),window_start_at DATETIME(6),window_end_at DATETIME(6),cpu_sampled_at DATETIME(6),cpu_window_seconds DECIMAL(18,6)",
		"instance_gpu_metric_samples": "id BIGINT PRIMARY KEY,sample_id BIGINT",
		"instance_metric_rollups":     "id BIGINT PRIMARY KEY,instance_id BIGINT,node_id BIGINT,bucket_start_at DATETIME(6),bucket_seconds BIGINT,coverage_intervals JSON",
		"instance_gpu_metric_rollups": "id BIGINT PRIMARY KEY,instance_id BIGINT,node_id BIGINT,bucket_start_at DATETIME(6),bucket_seconds BIGINT,coverage_intervals JSON",
		"metric_gaps":                 "id BIGINT PRIMARY KEY,node_id BIGINT,from_at DATETIME(6),to_at DATETIME(6),reason VARCHAR(64)",
		"metric_gpu_high_windows":     "node_id BIGINT,gpu_uuid VARCHAR(128),policy_version_id BIGINT NOT NULL,high_since DATETIME(6),last_sampled_at DATETIME(6),updated_at DATETIME(6),PRIMARY KEY(node_id,gpu_uuid,policy_version_id)",
	}
	for table, definition := range definitions {
		if err := tx.Exec("CREATE TEMPORARY TABLE `" + table + "` (" + definition + ") ENGINE=InnoDB").Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, statement := range []string{
		`INSERT INTO policy_versions VALUES
		(1,'metrics','{"raw_retention_days":2,"rollup_retention_days":3,"bucket_seconds":60,"collection_interval_seconds":5,"collection_timeout_seconds":10,"max_concurrent_nodes":1}'),
		(2,'metrics','{"raw_retention_days":1,"rollup_retention_days":1,"bucket_seconds":300,"collection_interval_seconds":30}'),
		(3,'metrics','{"raw_retention_days":5,"rollup_retention_days":7,"bucket_seconds":3600,"collection_interval_seconds":60}'),
		(4,'gpu-idle','{"enabled":false}'),
		(5,'gpu-scheduling','{"source_high_load_pct":80,"poll_interval_seconds":10,"metrics_max_age_seconds":60}'),
		(6,'gpu-idle','{"enabled":true}'),
		(7,'gpu-scheduling','{"source_high_load_pct":95,"poll_interval_seconds":30,"metrics_max_age_seconds":120}')`,
		`INSERT INTO nodes VALUES (1,1)`,
		`INSERT INTO instances(id,node_id,user_id,policy_versions) VALUES
		(1,1,1,'{"metrics":"2","gpu-idle":"4","gpu-scheduling":"5"}'),
		(2,1,1,'{"metrics":"3","gpu-idle":"6","gpu-scheduling":"7"}')`,
	} {
		if err := tx.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	return platform.New(tx, platform.Config{})
}

func TestMySQLMonitoringBindingsAreResourceSpecific(t *testing.T) {
	a := explicitMonitoringApp(t)
	ctx := context.Background()
	policies, err := nodeMetricPolicies(ctx, a, 1)
	if err != nil {
		t.Fatal(err)
	}
	if policies.Node.RawDays != 2 || policies.Node.Seconds != 60 || policies.Instances[1].RawDays != 1 || policies.Instances[1].Seconds != 300 || policies.Instances[2].RawDays != 5 || policies.Instances[2].Seconds != 3600 {
		t.Fatalf("node or frozen instance metrics binding was substituted: %+v", policies)
	}
	for id, enabled := range map[uint64]bool{1: false, 2: true} {
		instance, err := platform.Get(a.DB, "instances", id, false)
		if err != nil {
			t.Fatal(err)
		}
		idle, err := a.InstancePolicy(ctx, instance, "gpu-idle")
		if err != nil || platform.Bool(idle["enabled"]) != enabled {
			t.Fatalf("instance %d idle binding: %+v, %v", id, idle, err)
		}
		scheduling, err := a.InstancePolicy(ctx, instance, "gpu-scheduling")
		want := uint64(80)
		if id == 2 {
			want = 95
		}
		if err != nil || platform.Uint(scheduling["source_high_load_pct"]) != want {
			t.Fatalf("instance %d scheduling binding: %+v, %v", id, scheduling, err)
		}
	}
	if err := a.DB.Exec(`UPDATE instances SET policy_versions=JSON_REMOVE(policy_versions,'$.metrics') WHERE id=1`).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := nodeMetricPolicies(ctx, a, 1); err == nil {
		t.Fatal("missing frozen instance metrics binding fell back to node policy")
	}
}

func TestMySQLCollectionUsesNodeMetricsBinding(t *testing.T) {
	a := explicitMonitoringApp(t)
	if err := a.DB.Exec(`UPDATE policy_versions SET config=JSON_SET(config,'$.max_concurrent_nodes',0) WHERE id=1`).Error; err != nil {
		t.Fatal(err)
	}
	if err := collectMetrics(context.Background(), a); err == nil || !strings.Contains(err.Error(), "Explicit metrics worker bounds") {
		t.Fatalf("collector did not resolve explicit node worker bounds: %v", err)
	}
	if err := a.DB.Exec("UPDATE nodes SET metrics_policy_id=999 WHERE id=1").Error; err != nil {
		t.Fatal(err)
	}
	if err := collectMetrics(context.Background(), a); err == nil || !strings.Contains(err.Error(), "explicit policy required: metrics") {
		t.Fatalf("collector accepted a missing node binding: %v", err)
	}
}

func TestMySQLMetricRetentionHonorsBothReadSurfaces(t *testing.T) {
	a := explicitMonitoringApp(t)
	now := a.Time()
	if err := a.DB.Exec("INSERT INTO node_metric_cursors VALUES (1,'*',?)", now).Error; err != nil {
		t.Fatal(err)
	}
	for _, size := range []int64{60, 300, 3600} {
		if err := a.DB.Exec("INSERT INTO metric_rollup_progress VALUES (1,?,?)", size, now).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, fixture := range []struct {
		id, instance uint64
		age          time.Duration
		size         int64
	}{{1, 1, 4 * 24 * time.Hour, 300}, {2, 2, 4 * 24 * time.Hour, 3600}, {3, 1, 36 * time.Hour, 300}, {4, 2, 8 * 24 * time.Hour, 3600}} {
		at := now.Add(-fixture.age)
		if err := a.DB.Table("instance_metric_samples").Create(platform.Row{"id": fixture.id, "instance_id": fixture.instance, "node_id": 1, "sampled_at": at, "window_start_at": at.Add(-5 * time.Second), "window_end_at": at}).Error; err != nil {
			t.Fatal(err)
		}
		if err := a.DB.Table("instance_gpu_metric_samples").Create(platform.Row{"id": fixture.id, "sample_id": fixture.id}).Error; err != nil {
			t.Fatal(err)
		}
		for _, table := range []string{"instance_metric_rollups", "instance_gpu_metric_rollups"} {
			if err := a.DB.Table(table).Create(platform.Row{"id": fixture.id, "instance_id": fixture.instance, "node_id": 1, "bucket_start_at": at, "bucket_seconds": fixture.size, "coverage_intervals": `[]`}).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := rollupRetention(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"instance_metric_samples", "instance_gpu_metric_samples", "instance_metric_rollups", "instance_gpu_metric_rollups"} {
		var ids []uint64
		if err := a.DB.Table(table).Order("id").Pluck("id", &ids).Error; err != nil {
			t.Fatal(err)
		}
		if len(ids) != 2 || ids[0] != 2 || ids[1] != 3 {
			t.Fatalf("%s retained %v, want long-instance and node-retained observations [2 3]", table, ids)
		}
	}
	policies, err := nodeMetricPolicies(context.Background(), a, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []uint64{1, 2} {
		coverage, err := histCoverage(a.DB, id, now.Add(-10*24*time.Hour), now, 0, 0, "", policies.Instances[id], now)
		if err != nil {
			t.Fatal(err)
		}
		ranges := coverage["available_range"].(platform.Row)
		raw := ranges["raw"].(platform.Row)
		rollups := ranges["rollups"].([]platform.Row)
		if id == 1 && (!platform.Stamp(raw["from_at"]).IsZero() || len(rollups) != 0) {
			t.Fatalf("short instance exposed observations retained only for node reads: %+v", ranges)
		}
		if id == 2 && (platform.Stamp(raw["from_at"]).IsZero() || len(rollups) != 1 || platform.Uint(rollups[0]["bucket_seconds"]) != 3600) {
			t.Fatalf("long instance lost its own retained metric surface: %+v", ranges)
		}
	}
}

func TestMySQLGPUHighWindowsAreSchedulingPolicySpecific(t *testing.T) {
	a := explicitMonitoringApp(t)
	now := a.Time()
	for _, id := range []uint64{5, 7} {
		row := platform.Row{"node_id": 1, "gpu_uuid": "GPU-shared", "policy_version_id": id, "high_since": now.Add(-time.Minute), "last_sampled_at": now, "updated_at": now}
		if err := a.DB.Table("metric_gpu_high_windows").Clauses(gpuHighWindowConflict).Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := a.DB.Table("metric_gpu_high_windows").Clauses(gpuHighWindowConflict).Create(platform.Row{"node_id": 1, "gpu_uuid": "GPU-shared", "policy_version_id": 5, "high_since": nil, "last_sampled_at": now, "updated_at": now}).Error; err != nil {
		t.Fatal(err)
	}
	var rows []platform.Row
	if err := a.DB.Table("metric_gpu_high_windows").Order("policy_version_id").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0]["high_since"] != nil || platform.Stamp(rows[1]["high_since"]).IsZero() {
		t.Fatalf("one scheduling policy reset another policy's high window: %+v", rows)
	}
}

func TestMetricResolutionUsesFrozenBucketSize(t *testing.T) {
	for _, fixture := range []struct {
		resolution string
		seconds    int64
		wantSource int64
		wantError  bool
	}{{"hour", 300, 300, false}, {"hour", 3600, 3600, false}, {"minute", 300, 0, true}, {"auto", 3600, 0, false}} {
		c, _ := gin.CreateTestContext(nil)
		c.Request = httptest.NewRequest("GET", "/?resolution="+fixture.resolution, nil)
		from := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
		_, _, source, err := histResolution(c, from, from.Add(24*time.Hour), rollupPolicy{Seconds: fixture.seconds})
		if (err != nil) != fixture.wantError || source != fixture.wantSource {
			t.Fatalf("resolution %s with frozen bucket %d: source %d, error %v", fixture.resolution, fixture.seconds, source, err)
		}
	}
}
