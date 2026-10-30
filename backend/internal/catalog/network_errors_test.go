package catalog

import (
	"errors"
	"net/netip"
	"testing"

	"101cloud/backend/internal/platform"
)

func TestAddressRegistrationIdentifiesRejectedInput(t *testing.T) {
	pool := platform.Row{"id": uint64(1), "address_family": 4, "network_address": netip.MustParseAddr("192.0.2.0").AsSlice(), "prefix_length": 24, "range_start": netip.MustParseAddr("192.0.2.10").AsSlice(), "range_end": netip.MustParseAddr("192.0.2.20").AsSlice(), "gateway": netip.MustParseAddr("192.0.2.1").AsSlice()}
	for _, tc := range []struct{ address, reason string }{
		{"not-an-ip", "invalid_address"},
		{"::ffff:192.0.2.11", "invalid_address"},
		{"192.0.3.11", "outside_subnet"},
		{"192.0.2.1", "gateway"},
		{"192.0.2.0", "reserved_address"},
		{"192.0.2.255", "reserved_address"},
		{"192.0.2.9", "outside_range"},
		{"192.0.2.11", "duplicate_in_batch"},
	} {
		t.Run(tc.reason+tc.address, func(t *testing.T) {
			rows, err := networkAddressRows(pool, []string{"192.0.2.11", tc.address})
			var api *platform.HTTPError
			if rows != nil || !errors.As(err, &api) || api.Status != 422 || api.Code != "invalid_network" {
				t.Fatalf("unexpected rejection: %v %v", rows, err)
			}
			details, ok := api.Details.(platform.Row)
			if !ok {
				t.Fatalf("missing structured details: %v", api.Details)
			}
			for key, want := range map[string]string{"field": "addresses[1]", "address": tc.address, "reason": tc.reason, "range_start": "192.0.2.10", "range_end": "192.0.2.20", "subnet": "192.0.2.0/24"} {
				if details[key] != want {
					t.Fatalf("%s: got %v, want %s", key, details[key], want)
				}
			}
			if details["action"] == "" || details["action"] == nil {
				t.Fatal("missing remediation")
			}
		})
	}
	rows, err := networkAddressRows(pool, []string{"192.0.2.10", "192.0.2.20"})
	if err != nil || len(rows) != 2 {
		t.Fatalf("valid boundary addresses rejected: %v %v", rows, err)
	}
}

func TestAdministrativeGPUCapacityDetails(t *testing.T) {
	err := catalogCapacityError("gpu_in_use", "gpu", "gpus", "GPU 分配上限", "count", 4, 0, 2, "max_allocations", "请先释放分配。")
	var api *platform.HTTPError
	if !errors.As(err, &api) || api.Status != 409 || api.Code != "gpu_in_use" {
		t.Fatalf("unexpected error: %v", err)
	}
	details := api.Details.(platform.Row)
	for key, want := range map[string]string{"used": "4", "requested": "0", "limit": "2", "available": "0", "field": "max_allocations"} {
		if details[key] != want {
			t.Fatalf("%s: got %v, want %s", key, details[key], want)
		}
	}
}
