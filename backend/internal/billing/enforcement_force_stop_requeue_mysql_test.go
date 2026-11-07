package billing

import (
	"context"
	"testing"
	"time"

	"101cloud/backend/internal/platform"
	"gorm.io/gorm"
)

func TestMySQLAdministratorStoppedEnforcementDoesNotAutomaticallyRequeue(t *testing.T) {
	f := newAdjustmentMySQLFixture(t)
	f.addDebt(t, []string{"3.000003"})
	before := debtRow(t, f.db, "usage_charges", 1)
	if err := f.db.Exec(`INSERT INTO instance_plans(id,code,version,name,image_type,vcpus,memory_bytes,disk_bytes,gpu_count,pricing_version_id,credit_policy_version_id,idle_policy_version_id,init_parameters,allow_residency,status,allowed_overrides,policy_versions,gpu_policy,allowed_user_actions,gpu_user_permissions) VALUES(1,'force-stop-plan','1','Force stop plan','container',1,1073741824,1073741824,0,1,1,1,JSON_OBJECT(),FALSE,'active',JSON_OBJECT(),JSON_OBJECT('credit','1','metrics','100'),JSON_OBJECT(),JSON_ARRAY(),JSON_OBJECT())`).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Table("instances").Where("id=1").Updates(row{"product_type": "elastic", "instance_plan_id": 1, "plan_snapshot": jsonValue(row{"revision": "1", "vcpus": 1, "memory_bytes": "1073741824", "disk_bytes": "1073741824", "allowed_overrides": row{}}), "observed_state": "running", "observed_at": now(), "desired_state": "stopped"}).Error; err != nil {
		t.Fatal(err)
	}
	event, err := insert(f.db, "credit_enforcements", row{"account_id": 1, "user_id": 2, "policy_version_id": 1, "status": "stopping", "exhausted_at": now().Add(-time.Hour), "shutdown_deadline": now().Add(-time.Minute), "policy_snapshot": jsonValue(validCreditPolicy())})
	if err != nil {
		t.Fatal(err)
	}
	stopped, err := platform.Enqueue(f.db, "instance.stop", 2, 1, 1, false, row{"reason": "credit-enforcement:" + sid(event["id"])})
	if err != nil {
		t.Fatal(err)
	}
	taskID := platform.Uint(stopped["task_id"])
	if err = f.db.Table("tasks").Where("id=?", taskID).Updates(row{"status": "cancelled", "error_code": "task_force_stopped", "finished_at": now()}).Error; err != nil {
		t.Fatal(err)
	}
	member, err := insert(f.db, "credit_enforcement_instances", row{"enforcement_id": event["id"], "instance_id": 1, "status": "queued", "task_id": taskID})
	if err != nil {
		t.Fatal(err)
	}
	previous := stopHook
	calls := 0
	stopHook = func(_ context.Context, _ *platform.App, _ *gorm.DB, _, _ uint64, _ bool, _ string) (row, error) {
		calls++
		return row{}, nil
	}
	defer func() { stopHook = previous }()
	a := platform.New(f.db, platform.Config{})
	for range 2 {
		if err = advanceEvent(f.ctx, a, platform.Uint(event["id"])); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 0 || platform.Uint(debtRow(t, f.db, "credit_enforcement_instances", platform.Uint(member["id"]))["task_id"]) != taskID || platform.Text(debtRow(t, f.db, "credit_enforcements", platform.Uint(event["id"]))["status"]) != "unknown" {
		t.Fatal("administrator stop was automatically replaced or misreported as physically stopped")
	}
	reconcile, err := platform.Enqueue(f.db, "credit_enforcement_reconcile", 2, 0, 0, false, row{"enforcement_id": platform.Uint(event["id"])})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.db.Table("tasks").Where("id=?", reconcile["task_id"]).Updates(row{"status": "cancelled", "error_code": "task_force_stopped", "finished_at": now()}).Error; err != nil {
		t.Fatal(err)
	}
	beforeTasks := f.count(t, "tasks", "1=1")
	for range 2 {
		if err = queueEventReconcile(f.db, event); err != nil {
			t.Fatal(err)
		}
	}
	if f.count(t, "tasks", "1=1") != beforeTasks {
		t.Fatal("force-stopped credit reconciliation was automatically requeued")
	}
	assertDebtUnchanged(t, f.db, before)
	if f.count(t, "credit_ledger", "1=1") != 0 || f.count(t, "usage_sessions", "status='active'") != 1 {
		t.Fatal("administrator stop forgave debt or stopped billing")
	}
}
