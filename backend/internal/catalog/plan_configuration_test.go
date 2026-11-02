package catalog

import (
	"testing"

	"101cloud/backend/internal/platform"
)

func planPolicyFixture() map[string]platform.Row {
	return map[string]platform.Row{"gpu-idle": {"config": `{"enabled":true,"default_timeout_seconds":900,"min_timeout_seconds":60,"max_timeout_seconds":3600,"allowed_actions":["release_gpu","stop_instance"]}`}}
}

func TestPlanGPUConfigurationDefaultsAndExplicitEmptyPermissions(t *testing.T) {
	b := planInput{InstanceType: "container", GPU: GPURequest{MaxHourlyPT: "3"}}
	cfg, actions, permissions, err := planGPUConfiguration(b, planPolicyFixture())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Idle.Enabled || cfg.Idle.TimeoutSeconds != 900 || cfg.Idle.Action != "detach_gpu" || cfg.Rebalance.Enabled || cfg.Rebalance.AllowLive || cfg.Rebalance.MaxHourlyPT != "3.000000" {
		t.Fatalf("unsafe or unpinned defaults: %+v", cfg)
	}
	if len(actions) != 4 || len(permissions) != 0 {
		t.Fatalf("unexpected default permissions: %v %v", actions, permissions)
	}
	b.AllowedUserActions = []string{}
	b.GPUUserPermissions = map[string]bool{}
	_, actions, permissions, err = planGPUConfiguration(b, planPolicyFixture())
	if err != nil || actions == nil || len(actions) != 0 || permissions == nil || len(permissions) != 0 {
		t.Fatalf("explicit empty permissions were not retained: %v %v %v", actions, permissions, err)
	}
}

func policyPlanInput(t *testing.T) planInput {
	t.Helper()
	var b planInput
	err := decodeMap(platform.Row{
		"instance_type": "container", "gpu": platform.Row{"max_hourly_pt": "3"},
		"gpu_policy": platform.Row{
			"idle":      platform.Row{"enabled": true, "timeout_seconds": 900, "action": "detach_gpu"},
			"rebalance": platform.Row{"enabled": true, "allow_live": true, "max_hourly_pt": "3.000000"},
		},
		"allowed_user_actions": []string{"start", "gpu_attach", "gpu_policy"},
		"gpu_user_permissions": map[string]bool{"idle": true, "max_hourly_pt": true},
	}, &b)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPlanGPUConfigurationValidatesPolicyAndCapabilities(t *testing.T) {
	b := policyPlanInput(t)
	if _, _, _, err := planGPUConfiguration(b, planPolicyFixture()); err != nil {
		t.Fatalf("valid explicit policy rejected: %v", err)
	}
	for name, change := range map[string]func(*planInput){
		"mismatched-cap":                 func(b *planInput) { b.GPUPolicy.Rebalance.MaxHourlyPT = "3.000001" },
		"out-of-range-timeout":           func(b *planInput) { *b.GPUPolicy.Idle.TimeoutSeconds = 59 },
		"disabled-timeout-still-bounded": func(b *planInput) { *b.GPUPolicy.Idle.Enabled = false; *b.GPUPolicy.Idle.TimeoutSeconds = 3601 },
		"unknown-action":                 func(b *planInput) { b.GPUPolicy.Idle.Action = "release_gpu" },
		"partial-policy":                 func(b *planInput) { b.GPUPolicy.Rebalance.AllowLive = nil },
		"duplicate-action":               func(b *planInput) { b.AllowedUserActions = []string{"start", "start"} },
		"unknown-user-action":            func(b *planInput) { b.AllowedUserActions = []string{"root"} },
		"unknown-permission":             func(b *planInput) { b.GPUUserPermissions = map[string]bool{"root": false} },
		"vm-enabled-policy": func(b *planInput) {
			b.InstanceType = "virtual-machine"
			b.AllowedUserActions = nil
			b.GPUUserPermissions = nil
		},
		"vm-gpu-action":     func(b *planInput) { b.InstanceType = "virtual-machine"; b.GPUPolicy = nil; b.GPUUserPermissions = nil },
		"vm-gpu-permission": func(b *planInput) { b.InstanceType = "virtual-machine"; b.GPUPolicy = nil; b.AllowedUserActions = nil },
	} {
		t.Run(name, func(t *testing.T) {
			b := policyPlanInput(t)
			change(&b)
			_, _, _, err := planGPUConfiguration(b, planPolicyFixture())
			requireValidation(t, err)
		})
	}
	policies := planPolicyFixture()
	policies["gpu-idle"]["config"] = `{"enabled":false,"default_timeout_seconds":900,"min_timeout_seconds":60,"max_timeout_seconds":3600,"allowed_actions":["stop_instance"]}`
	_, _, _, err := planGPUConfiguration(b, policies)
	requireValidation(t, err)
}

func TestPlanMoneyUsesExactDatabaseRange(t *testing.T) {
	for _, value := range []string{"0", "0.000001", "99999999999999.999999"} {
		if _, err := planMoney(value); err != nil {
			t.Fatalf("valid exact price %q rejected: %v", value, err)
		}
	}
	for _, value := range []string{"", "-1", "+1", "1e2", "1.0000001", "100000000000000", " 1", "1 ", "1.", "."} {
		_, err := planMoney(value)
		requireValidation(t, err)
	}
}
