package instances

import (
	"101cloud/backend/internal/platform"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	var api *platform.HTTPError
	if !errors.As(err, &api) || api.Code != code {
		t.Fatalf("expected %s, got %v", code, err)
	}
}
func TestConfiguredGPUUUIDsResolvesPhysicalIdentity(t *testing.T) {
	native := platform.Row{"expanded_devices": platform.Row{"root": platform.Row{"type": "disk"}, "gpu": platform.Row{"type": "gpu", "gputype": "physical", "pci": "41:00.0"}}}
	inventory := platform.Row{"gpus": []any{platform.Row{"uuid": "GPU-first", "pci_address": "0000:41:00.0"}}}
	ids, err := configuredGPUUUIDs(native, inventory)
	if err != nil || len(ids) != 1 || ids[0] != "GPU-first" {
		t.Fatalf("unexpected resolved identity: %v, %v", ids, err)
	}
}
func TestConfiguredGPUUUIDsRejectsIncompleteEvidence(t *testing.T) {
	cases := []struct {
		name              string
		native, inventory platform.Row
		code              string
	}{
		{"missing-expanded", platform.Row{}, platform.Row{}, "observation_incomplete"},
		{"malformed-expanded", platform.Row{"expanded_devices": "{}"}, platform.Row{}, "observation_incomplete"},
		{"missing-device-type", platform.Row{"expanded_devices": platform.Row{"gpu": platform.Row{"pci": "41:00.0"}}}, platform.Row{}, "observation_incomplete"},
		{"unresolved-pci", platform.Row{"expanded_devices": platform.Row{"gpu": platform.Row{"type": "gpu", "pci": "41:00.0"}}}, platform.Row{}, "observation_incomplete"},
		{"nonphysical", platform.Row{"expanded_devices": platform.Row{"gpu": platform.Row{"type": "gpu", "gputype": "mig", "pci": "41:00.0"}}}, platform.Row{}, "unsupported_configuration"},
		{"duplicate-pci", platform.Row{"expanded_devices": platform.Row{}}, platform.Row{"gpus": []any{platform.Row{"uuid": "GPU-a", "pci_address": "41:00.0"}, platform.Row{"uuid": "GPU-b", "pci_address": "41:00.0"}}}, "observation_incomplete"},
		{"duplicate-uuid", platform.Row{"expanded_devices": platform.Row{}}, platform.Row{"gpus": []any{platform.Row{"uuid": "GPU-a", "pci_address": "41:00.0"}, platform.Row{"uuid": "GPU-a", "pci_address": "42:00.0"}}}, "observation_incomplete"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := configuredGPUUUIDs(tc.native, tc.inventory)
			requireCode(t, err, tc.code)
		})
	}
}
func idleCard() platform.Row {
	return platform.Row{"state": "idle_candidate", "configured": true, "observed": false, "process_count": 0, "referencing_process_count": 0, "used_memory_bytes": uint64(0), "quality": platform.Row{"attribution_complete": true}}
}
func TestGPUIdleRequiresCompleteReferenceEvidence(t *testing.T) {
	if err := safeLiveCard(idleCard(), false); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"process_count", "referencing_process_count", "used_memory_bytes"} {
		t.Run(field, func(t *testing.T) {
			card := idleCard()
			card[field] = nil
			requireCode(t, safeLiveCard(card, false), "observation_incomplete")
		})
	}
	card := idleCard()
	card["quality"] = platform.Row{"attribution_complete": false}
	requireCode(t, safeLiveCard(card, false), "observation_incomplete")
	card = idleCard()
	card["referencing_process_count"] = 1
	requireCode(t, safeLiveCard(card, false), "gpu_in_use")
	card = idleCard()
	card["used_memory_bytes"] = 1
	requireCode(t, safeLiveCard(card, false), "gpu_in_use")
}
func TestGPUReleaseRequiresConfigurationAndOccupancyGone(t *testing.T) {
	card := idleCard()
	requireCode(t, safeLiveCard(card, true), "observation_incomplete")
	card["configured"] = false
	card["observed"] = true
	requireCode(t, safeLiveCard(card, true), "observation_incomplete")
	card["observed"] = false
	if err := safeLiveCard(card, true); err != nil {
		t.Fatal(err)
	}
}
func TestResourcesRejectInvalidCapacityStrings(t *testing.T) {
	for _, r := range []Resources{{0, "1", "1"}, {1, "0", "1"}, {1, "-1", "1"}, {1, "1.5", "1"}, {1, "1", "18446744073709551616"}, {1, "1", "0"}} {
		if _, _, err := validResources(r); err == nil {
			t.Fatalf("accepted invalid resources: %+v", r)
		}
	}
	memory, disk, err := validResources(Resources{4, "8589934592", "96636764160"})
	if err != nil || memory != 8589934592 || disk != 96636764160 {
		t.Fatalf("unexpected resources: %d, %d, %v", memory, disk, err)
	}
}
func TestImportUUIDListsPreserveStringIdentity(t *testing.T) {
	ids := rows([]string{"GPU-one", "GPU-two"})
	if len(ids) != 2 || ids[0] != "GPU-one" || ids[1] != "GPU-two" {
		t.Fatalf("lost imported GPU identities: %v", ids)
	}
}

func TestGPURequestRejectsExternalControlReasons(t *testing.T) {
	decode := func(body string, target any) error {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		return platform.Decode(c, target)
	}
	var request GPURequest
	if err := decode(`{"count":1,"model":"A100","access_mode":"shared","max_hourly_pt":"1.000000"}`, &request); err != nil {
		t.Fatalf("reasonless GPU request rejected: %v", err)
	}
	for _, reason := range []string{"administrator note", "automatic_gpu_idle", "automatic_gpu_rebalance", "credit-enforcement:1"} {
		t.Run(reason, func(t *testing.T) {
			payload, err := json.Marshal(map[string]any{"count": 1, "reason": reason})
			if err != nil {
				t.Fatal(err)
			}
			var direct GPURequest
			requireCode(t, decode(string(payload), &direct), "validation_failed")
			var nested Spec
			requireCode(t, decode(`{"gpu":`+string(payload)+`}`, &nested), "validation_failed")
		})
	}
	request.Reason = "automatic_gpu_rebalance"
	payload, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(payload, &fields); err != nil {
		t.Fatal(err)
	}
	if _, exists := fields["reason"]; exists {
		t.Fatalf("internal control reason leaked into public GPU configuration: %s", payload)
	}
}

func TestAdminGPUPolicyAcceptsNoOperationReason(t *testing.T) {
	for _, body := range []string{
		`{"idle":{"enabled":false,"timeout_seconds":1800,"action":"detach_gpu"},"rebalance":{"enabled":false,"allow_live":false,"max_hourly_pt":"1.000000"}}`,
		`{"idle":{},"rebalance":{},"reason":"automatic_gpu_rebalance"}`,
	} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPut, "/", strings.NewReader(body))
		var input adminGPUPolicyInput
		err := platform.Decode(c, &input)
		if strings.Contains(body, `"reason"`) {
			requireCode(t, err, "validation_failed")
		} else if err != nil {
			t.Fatalf("reasonless administrator GPU policy rejected: %v", err)
		}
	}
}
