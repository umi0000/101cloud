package catalog

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"101cloud/backend/internal/platform"
)

func requireValidation(t *testing.T, err error) {
	t.Helper()
	var api *platform.HTTPError
	if !errors.As(err, &api) || api.Status != 422 || api.Code != "validation_failed" {
		t.Fatalf("expected 422 validation_failed, got %v", err)
	}
}

func gpuPlan(base uint64, overrides string) platform.Row {
	return platform.Row{"image_type": "container", "vcpus": uint64(2), "memory_bytes": "4294967296", "disk_bytes": "21474836480", "gpu_count": base, "gpu_model": "NVIDIA A100", "gpu_access_mode": "exclusive", "gpu_max_hourly_pt": "3.000000", "allowed_overrides": overrides}
}

var planResources = Resources{VCPUs: 2, MemoryBytes: "4294967296", DiskBytes: "21474836480"}

func checkRequest(plan platform.Row, g GPURequest) error {
	if e := validGPU(g, platform.Text(plan["image_type"])); e != nil {
		return e
	}
	return validateOverrides(plan, planResources, g)
}

func TestElasticGPUCountZeroOptsOutOfPlanGPUs(t *testing.T) {
	optOut := GPURequest{Count: 0, AccessMode: "shared", MaxHourlyPT: "0"}
	for name, plan := range map[string]platform.Row{
		"base-gpu-without-override": gpuPlan(1, `{}`),
		"override-min-above-zero":   gpuPlan(2, `{"gpu_count":{"min":"2","max":"4","step":"2"}}`),
		"optional-offering":         gpuPlan(0, `{"gpu_count":{"min":"1","max":"4","step":"1"}}`),
	} {
		if e := checkRequest(plan, optOut); e != nil {
			t.Fatalf("%s: opt-out rejected: %v", name, e)
		}
	}
	model := "NVIDIA A100"
	requireValidation(t, checkRequest(gpuPlan(1, `{}`), GPURequest{Count: 0, Model: &model, AccessMode: "exclusive", MaxHourlyPT: "3"}))
}

func TestElasticGPUCountMustMatchPlanOffering(t *testing.T) {
	model, other := "NVIDIA A100", "NVIDIA H100"
	plan := gpuPlan(0, `{"gpu_count":{"min":"1","max":"4","step":"1"}}`)
	if e := checkRequest(plan, GPURequest{Count: 2, Model: &model, AccessMode: "exclusive", MaxHourlyPT: "3"}); e != nil {
		t.Fatalf("in-range count rejected: %v", e)
	}
	for name, g := range map[string]GPURequest{
		"above-range":    {Count: 5, Model: &model, AccessMode: "exclusive", MaxHourlyPT: "3"},
		"missing-model":  {Count: 1, AccessMode: "exclusive", MaxHourlyPT: "3"},
		"other-model":    {Count: 1, Model: &other, AccessMode: "exclusive", MaxHourlyPT: "3"},
		"other-mode":     {Count: 1, Model: &model, AccessMode: "shared", MaxHourlyPT: "3"},
		"price-too-high": {Count: 1, Model: &model, AccessMode: "exclusive", MaxHourlyPT: "3.5"},
	} {
		t.Run(name, func(t *testing.T) { requireValidation(t, checkRequest(plan, g)) })
	}
	requireValidation(t, checkRequest(gpuPlan(1, `{}`), GPURequest{Count: 2, Model: &model, AccessMode: "exclusive", MaxHourlyPT: "3"}))
	noGPU := platform.Row{"image_type": "container", "vcpus": uint64(2), "memory_bytes": "4294967296", "disk_bytes": "21474836480", "gpu_count": uint64(0), "gpu_model": nil, "gpu_access_mode": "shared", "gpu_max_hourly_pt": "0", "allowed_overrides": `{}`}
	requireValidation(t, checkRequest(noGPU, GPURequest{Count: 1, Model: &model, AccessMode: "shared", MaxHourlyPT: "0"}))
	vm := platform.Row{"image_type": "virtual-machine", "vcpus": uint64(2), "memory_bytes": "4294967296", "disk_bytes": "21474836480", "gpu_count": uint64(0), "gpu_model": nil, "gpu_access_mode": "shared", "gpu_max_hourly_pt": "0", "allowed_overrides": `{}`}
	requireValidation(t, checkRequest(vm, GPURequest{Count: 1, Model: &model, AccessMode: "shared", MaxHourlyPT: "0"}))
	if e := checkRequest(vm, GPURequest{Count: 0, AccessMode: "shared", MaxHourlyPT: "0"}); e != nil {
		t.Fatalf("CPU-only VM request rejected: %v", e)
	}
}

func TestPlanDefinitionAcceptsOptionalGPUOffering(t *testing.T) {
	model := "NVIDIA A100"
	optional := map[string]any{"gpu_count": map[string]any{"min": "1", "max": "4", "step": "1"}}
	g := GPURequest{Count: 0, Model: &model, AccessMode: "exclusive", MaxHourlyPT: "3"}
	if e := validateOverrideDefinition(optional, planResources, g); e != nil {
		t.Fatalf("optional GPU override rejected: %v", e)
	}
	if e := validPlanGPU(g, "container", 4); e != nil {
		t.Fatalf("optional GPU offering rejected: %v", e)
	}
	requireValidation(t, validPlanGPU(g, "container", 0))
	requireValidation(t, validPlanGPU(g, "virtual-machine", 4))
	requireValidation(t, validPlanGPU(GPURequest{Count: 0, AccessMode: "shared", MaxHourlyPT: "0"}, "virtual-machine", 1))
	requireValidation(t, validPlanGPU(GPURequest{Count: 0, AccessMode: "shared", MaxHourlyPT: "0"}, "container", 1))
	if e := validPlanGPU(GPURequest{Count: 0, AccessMode: "shared", MaxHourlyPT: "0"}, "virtual-machine", 0); e != nil {
		t.Fatalf("CPU-only VM plan rejected: %v", e)
	}
}

func TestMySQLQuoteUsesReadyImageForNetworkProject(t *testing.T) {
	ctx, root := openCatalogMySQL(t, "ci_quote_images_")
	db := root.Begin()
	if db.Error != nil {
		t.Fatal(db.Error)
	}
	t.Cleanup(func() { db.Rollback() })
	for table, definition := range map[string]string{
		"instance_plans":        "id BIGINT PRIMARY KEY,status VARCHAR(24),image_type VARCHAR(24),vcpus BIGINT,memory_bytes BIGINT,disk_bytes BIGINT,gpu_count BIGINT,gpu_model VARCHAR(64),gpu_access_mode VARCHAR(24),gpu_max_hourly_pt VARCHAR(24),allowed_overrides JSON,policy_versions JSON,init_template_version_id BIGINT",
		"plan_images":           "plan_id BIGINT,image_id BIGINT",
		"plan_network_options":  "plan_id BIGINT,network_option_id BIGINT",
		"images":                "id BIGINT PRIMARY KEY,status VARCHAR(24),image_type VARCHAR(24),architecture VARCHAR(24),fingerprint BINARY(32),gpu_ready BOOL,properties JSON",
		"image_nodes":           "id BIGINT PRIMARY KEY,image_id BIGINT,node_id BIGINT,project_name VARCHAR(64),architecture VARCHAR(24),fingerprint BINARY(32),gpu_ready BOOL,cloud_init BOOL,properties JSON,status VARCHAR(24),source_id VARCHAR(24),variant VARCHAR(64),revision BIGINT,last_verified_at DATETIME(6)",
		"node_network_bindings": "id BIGINT PRIMARY KEY,node_id BIGINT,project_name VARCHAR(64),network_pool_id BIGINT,status VARCHAR(24),validation_status VARCHAR(24),mode VARCHAR(24)",
		"network_pools":         "id BIGINT PRIMARY KEY,status VARCHAR(24)",
		"nodes":                 "id BIGINT PRIMARY KEY,resources JSON,default_project_name VARCHAR(64)",
		"policy_versions":       "id BIGINT PRIMARY KEY,kind VARCHAR(24),source_id BIGINT,status VARCHAR(24),effective_at DATETIME(6)",
	} {
		if err := db.Exec("CREATE TEMPORARY TABLE `" + table + "` (" + definition + ") ENGINE=InnoDB").Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, statement := range []string{
		`INSERT INTO instance_plans VALUES(1,'active','container',2,4294967296,21474836480,1,'NVIDIA A100','exclusive','3',JSON_OBJECT(),'{"pricing":"1","credit":"2","gpu-idle":"3","gpu-scheduling":"4","gpu-pricing":"5"}',NULL)`,
		`INSERT INTO plan_images VALUES(1,1)`,
		`INSERT INTO plan_network_options VALUES(1,1)`,
		`INSERT INTO images VALUES(1,'active','container','aarch64',UNHEX(REPEAT('bb',32)),FALSE,JSON_OBJECT())`,
		`INSERT INTO nodes VALUES(1,'{"architecture":"x86_64"}','default')`,
		`INSERT INTO network_pools VALUES(1,'active')`,
		`INSERT INTO node_network_bindings VALUES(1,1,'tenant-project',1,'active','verified','bridge')`,
		`INSERT INTO image_nodes VALUES(1,1,1,'default','aarch64',UNHEX(REPEAT('bb',32)),FALSE,FALSE,JSON_OBJECT(),'ready',NULL,'default',1,NOW(6)),(2,1,1,'tenant-project','x86_64',UNHEX(REPEAT('aa',32)),TRUE,TRUE,JSON_OBJECT(),'ready',NULL,'cloud',1,NOW(6))`,
		`INSERT INTO policy_versions VALUES(1,'pricing',1,'active',NOW()-INTERVAL 1 HOUR),(2,'credit',1,'active',NOW()-INTERVAL 1 HOUR),(3,'gpu-idle',1,'active',NOW()-INTERVAL 1 HOUR),(4,'gpu-scheduling',1,'active',NOW()-INTERVAL 1 HOUR),(5,'gpu-pricing',1,'active',NOW()-INTERVAL 1 HOUR)`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	model := "NVIDIA A100"
	spec := CreateElasticSpec{Name: "Project image", PlanID: "1", ImageID: "1", NetworkOptionID: "1", Resources: planResources, GPU: GPURequest{Count: 1, Model: &model, AccessMode: "exclusive", MaxHourlyPT: "3"}, InitParameters: map[string]any{}}
	a := platform.New(db, platform.Config{})
	validated, err := ValidateElastic(ctx, db, a, spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	if platform.Text(validated.Image["architecture"]) != "x86_64" || !platform.Bool(validated.Image["gpu_ready"]) || workerQuoteFingerprint(validated.Image["fingerprint"]) != strings.Repeat("aa", 32) {
		t.Fatalf("quote did not freeze selected project's metadata: %+v", validated.Image)
	}
	for _, status := range []string{"pending", "downloading", "failed", "unknown"} {
		if err := db.Table("image_nodes").Where("id=2").Update("status", status).Error; err != nil {
			t.Fatal(err)
		}
		if _, err := ValidateElastic(ctx, db, a, spec, nil); err == nil {
			t.Fatalf("quote accepted %s image by falling back to default project", status)
		}
	}
}

func workerQuoteFingerprint(v any) string {
	if b, ok := v.([]byte); ok {
		return fmt.Sprintf("%x", b)
	}
	return ""
}
