package instances

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"101cloud/backend/internal/platform"
)

func TestDiskShortageHasActualExactCapacity(t *testing.T) {
	const gib = uint64(1024 * 1024 * 1024)
	err := storageCapacityError(map[string]any{"total": json.Number("20401094656"), "used": json.Number("4294967296")}, "default", 20*gib)
	var he *platform.HTTPError
	if !errors.As(err, &he) || he.Status != 409 || he.Code != "storage_pool_capacity_exceeded" {
		t.Fatalf("shortage not typed: %v", err)
	}
	details := he.Details.(platform.Row)
	for k, want := range map[string]string{"scope": "storage_pool", "resource": "disk_bytes", "pool": "default", "unit": "bytes", "requested": "21474836480", "available": "16106127360", "total": "20401094656", "limit": "20401094656", "used": "4294967296"} {
		if details[k] != want {
			t.Errorf("%s=%v want %s", k, details[k], want)
		}
	}
	if storageCapacityError(map[string]any{"total": "18446744073709551615", "used": "18446744073709551614"}, "large", 1) != nil {
		t.Fatal("uint64 capacity lost precision")
	}
	if storageCapacityError(map[string]any{"total": "20401094656", "used": "4294967296"}, "default", 15*gib) != nil {
		t.Fatal("exact available capacity rejected")
	}
	for _, space := range []map[string]any{nil, {"total": "invalid", "used": "0"}, {"total": "100", "used": "-1"}} {
		err := storageCapacityError(space, "default", 1)
		if !errors.As(err, &he) || he.Code != "storage_pool_capacity_unknown" {
			t.Fatalf("unknown measurement treated as zero: %v", err)
		}
	}
}

func TestCreationReleaseGuardRejectsRecoveredOrExistingState(t *testing.T) {
	job := platform.Job{ID: 43, Type: "instance.create", InstanceID: 15, UserID: 2, NodeID: 1}
	base := platform.Row{"id": uint64(15), "user_id": uint64(2), "node_id": uint64(1), "incus_uuid": nil, "deleted_at": nil, "provisioning_state": "pending", "observed_state": "pending"}
	if !workerPristineCreation(base, job) {
		t.Fatal("new pending creation rejected")
	}
	for _, change := range []platform.Row{{"incus_uuid": "guest-exists"}, {"observed_state": "running"}, {"observed_state": "stopped"}, {"provisioning_state": "unknown"}, {"user_id": uint64(3)}, {"node_id": uint64(2)}, {"deleted_at": "2026-10-31T00:00:00+08:00"}} {
		r := platform.Row{}
		for k, v := range base {
			r[k] = v
		}
		for k, v := range change {
			r[k] = v
		}
		if workerPristineCreation(r, job) {
			t.Fatalf("unsafe state authorized cleanup: %+v", change)
		}
	}
	job.Type = "instance.reconcile"
	if workerPristineCreation(base, job) {
		t.Fatal("reconciliation acquired fresh cleanup authority")
	}
}

func TestAmbiguousDispatchKeepsSafeConcreteCause(t *testing.T) {
	cause := platform.EDetails(409, "storage_pool_capacity_exceeded", "节点存储池空间不足。", platform.Row{"available": "1", "requested": "2"})
	retained := workerRetainedCreationError(cause)
	got := platform.PublicError(platform.Unknown(retained), true)
	if got["code"] != "storage_pool_capacity_exceeded" {
		t.Fatalf("unknown wrapper erased concrete cause: %+v", got)
	}
	details := got["details"].(map[string]any)
	if details["resource_reservations_released"] != false || details["action"] == "" {
		t.Fatalf("retention boundary absent: %+v", got)
	}
	timeout := platform.PublicError(platform.Unknown(context.DeadlineExceeded), true)
	if timeout["code"] != "external_result_unknown" {
		t.Fatalf("timeout became known failure: %+v", timeout)
	}
}
