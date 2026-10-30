package catalog

import (
	"errors"
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
