package billing

import (
	"errors"
	"sync"
	"testing"
	"time"

	"101cloud/backend/internal/platform"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

func failedBillingTask(t *testing.T, f *adjustmentMySQLFixture) time.Time {
	t.Helper()
	at := now()
	task := row{"id": 1, "task_type": "instance.create", "idempotency_scope": "failed-billing", "idempotency_key": "original-create", "user_id": 2, "node_id": 1, "instance_id": 1, "is_instance_mutation": 1, "status": "failed", "parameters_redacted": `{}`, "finished_at": at}
	if err := f.db.Table("tasks").Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Table("instances").Where("id=1").Updates(row{"creation_failed_at": at, "creation_failure_task_id": 1, "creation_cleanup_state": "pending"}).Error; err != nil {
		t.Fatal(err)
	}
	return at
}

func cancelFailedBillingChunk(f *adjustmentMySQLFixture, at time.Time) error {
	return f.db.WithContext(f.ctx).Transaction(func(db *gorm.DB) error {
		if _, err := account(db, 2); err != nil {
			return err
		}
		return CancelFailedCreation(f.ctx, db, row{"id": 1}, at)
	})
}

func failedBillingCharge(db *gorm.DB, session uint64, expected string, at time.Time) (row, error) {
	return insert(db, "usage_charges", row{"account_id": 1, "session_id": session, "from_at": at, "to_at": at.Add(time.Second), "expected_pt": expected, "settled_pt": "0.000000", "status": "pending", "calculation_snapshot": `{}`})
}

func failedBillingReservation(db *gorm.DB, bucket row, session uint64, value, key string, at time.Time) error {
	reservation, err := insert(db, "credit_reservations", row{"reservation_key": key, "account_id": 1, "session_id": session, "bucket_id": bucket["id"], "window_from": at, "window_to": at.Add(time.Hour), "reserved_at": at, "expires_at": at.Add(time.Hour), "reserved_pt": value, "settled_pt": "0.000000", "released_pt": "0.000000", "status": "held"})
	if err != nil {
		return err
	}
	return mutateBucket(db, bucket, "reserve:"+sid(reservation["id"]), "reserve", decimal.Zero, dec(value), row{"reservation_id": reservation["id"]})
}

func TestMySQLFailedCreationRefundPreservesSourcesHistoryAndOtherDebt(t *testing.T) {
	f := newAdjustmentMySQLFixture(t)
	at := now().Add(-48 * time.Hour)
	var originalCharge row
	var expiredID, liveID, heldID uint64
	instance := debtRow(t, f.db, "instances", 1)
	instance["id"], instance["logical_name"], instance["incus_name"] = 2, "Other instance", "other-failed-billing-instance"
	if err := f.db.Table("instances").Create(&instance).Error; err != nil {
		t.Fatal(err)
	}
	session := debtRow(t, f.db, "usage_sessions", 1)
	delete(session, "active_instance_id")
	delete(session, "allocation_key")
	session["id"], session["instance_id"] = 3, 2
	if err := f.db.Table("usage_sessions").Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Transaction(func(db *gorm.DB) error {
		ac, err := account(db, 2)
		if err != nil {
			return err
		}
		expiry := at.Add(time.Hour)
		expired, err := grant(db, ac, "admin", "failed-expired-source", dec("4"), at.Add(-time.Hour), &expiry, 1)
		if err != nil {
			return err
		}
		live, err := grant(db, ac, "admin", "failed-live-source", dec("2"), at.Add(-time.Hour), nil, 1)
		if err != nil {
			return err
		}
		expiredID, liveID = platform.Uint(expired["id"]), platform.Uint(live["id"])
		originalCharge, err = failedBillingCharge(db, 1, "9.000000", at)
		if err != nil {
			return err
		}
		if err = payCharge(db, ac, row{"id": 1}, originalCharge, at); err != nil {
			return err
		}
		held, err := grant(db, ac, "admin", "failed-shared-held-source", dec("5"), at.Add(-time.Hour), nil, 1)
		if err != nil {
			return err
		}
		heldID = platform.Uint(held["id"])
		if err = failedBillingReservation(db, held, 2, "3.000000", "failed-own-held", at); err != nil {
			return err
		}
		if err = failedBillingReservation(db, held, 3, "2.000000", "other-own-held", at); err != nil {
			return err
		}
		_, err = insert(db, "usage_charges", row{"account_id": 1, "session_id": 3, "from_at": at, "to_at": at.Add(time.Second), "expected_pt": "2.000000", "settled_pt": "0.000000", "status": "insufficient", "calculation_snapshot": `{}`})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	before := debtRow(t, f.db, "usage_charges", platform.Uint(originalCharge["id"]))
	failedAt := failedBillingTask(t, f)
	if err := cancelFailedBillingChunk(f, failedAt); err != nil {
		t.Fatal(err)
	}
	charge := debtRow(t, f.db, "usage_charges", platform.Uint(originalCharge["id"]))
	if platform.Text(charge["status"]) != "cancelled" || money(dec(charge["expected_pt"])) != "9.000000" || money(dec(charge["settled_pt"])) != "6.000000" || !dec(before["settled_pt"]).Equal(dec(charge["settled_pt"])) {
		t.Fatalf("failed charge history was rewritten: %#v", charge)
	}
	if got := debtRow(t, f.db, "credit_buckets", expiredID); !dec(got["remaining_pt"]).IsZero() || !dec(got["reserved_pt"]).IsZero() {
		t.Fatalf("expired refund became spendable: %#v", got)
	}
	if got := debtRow(t, f.db, "credit_buckets", liveID); money(dec(got["remaining_pt"])) != "2.000000" {
		t.Fatalf("live refund did not return to its source: %#v", got)
	}
	if got := debtRow(t, f.db, "credit_buckets", heldID); money(dec(got["reserved_pt"])) != "2.000000" {
		t.Fatalf("another instance lost its reservation: %#v", got)
	}
	if f.count(t, "usage_charges", "session_id=3 AND status='insufficient' AND expected_pt=2 AND settled_pt=0") != 1 || f.count(t, "credit_reservations", "session_id=2 AND status='released' AND released_pt=3") != 1 {
		t.Fatal("other debt changed or failed reservation was not released")
	}
	if err := f.db.Transaction(func(db *gorm.DB) error {
		if _, err := account(db, 2); err != nil {
			return err
		}
		debt, err := outstandingDebt(db, 1)
		if err == nil && money(debt) != "2.000000" {
			return errors.New("failed debt remained in account outstanding debt")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	pending, err := FailedCreationBillingPending(f.ctx, f.db, row{"id": 1})
	if err != nil || pending {
		t.Fatalf("completed financial cleanup still pending: %v %v", pending, err)
	}
	ledger := f.count(t, "credit_ledger", "1=1")
	if err = cancelFailedBillingChunk(f, failedAt); err != nil {
		t.Fatal(err)
	}
	if f.count(t, "credit_ledger", "1=1") != ledger || f.count(t, "credit_ledger", "entry_type='refund'") != 2 || f.count(t, "credit_ledger", "entry_type='settle'") != 2 {
		t.Fatal("retry duplicated reversals or removed original settlement history")
	}
}

func TestMySQLFailedCreationImmediatelyBlocksStaleUsageAndDebtBeforeChunks(t *testing.T) {
	f := newAdjustmentMySQLFixture(t)
	staleInstance := debtRow(t, f.db, "instances", 1)
	staleSession := debtRow(t, f.db, "usage_sessions", 2)
	f.addDebt(t, []string{"3.000000"})
	failedBillingTask(t, f)
	if err := f.db.Transaction(func(db *gorm.DB) error {
		ac, err := account(db, 2)
		if err != nil {
			return err
		}
		if _, err = grant(db, ac, "admin", "after-failed-mark", dec("20"), now(), nil, 1); err != nil {
			return err
		}
		p, err := instanceCreditPolicy(db, staleInstance)
		if err != nil {
			return err
		}
		if err = InstanceUsage(f.ctx, db, staleInstance, "running", nil); err != nil {
			return err
		}
		if err = beginSession(db, ac, p, staleInstance, "storage", nil, "running", now()); err != nil {
			return err
		}
		if err = settleSession(db, ac, p, staleSession, now().Add(time.Hour)); err != nil {
			return err
		}
		if err = reserveWindow(db, ac, p, staleSession, now(), true); err != nil {
			return err
		}
		if err = restore(db, ac, now()); err != nil {
			return err
		}
		charge, err := platform.Get(db, "usage_charges", 1, true)
		if err != nil {
			return err
		}
		if err = payCharge(db, ac, row{"id": 1}, charge, now()); err != nil {
			return err
		}
		debt, err := outstandingDebt(db, 1)
		if err == nil && !debt.IsZero() {
			return errors.New("marked failed creation still contributes payable debt")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if f.count(t, "credit_ledger", "entry_type IN ('settle','reserve')") != 0 || f.count(t, "usage_sessions", "1=1") != 2 || f.count(t, "usage_charges", "1=1") != 1 {
		t.Fatal("stale lifecycle/debt entrypoint restarted failed creation billing")
	}
	if pending, err := FailedCreationBillingPending(f.ctx, f.db, row{"id": 1}); err != nil || !pending {
		t.Fatalf("unprocessed cleanup not pending: %v %v", pending, err)
	}
}

func TestMySQLFailedCreationChunksRefundSingleChargeSourcesAcrossRestart(t *testing.T) {
	f := newAdjustmentMySQLFixture(t)
	at := now().Add(-time.Hour)
	count := settlementChunkSize*2 + 3
	if err := f.db.Transaction(func(db *gorm.DB) error {
		ac, err := account(db, 2)
		if err != nil {
			return err
		}
		bucket, err := grant(db, ac, "admin", "many-refund-sources", decimal.NewFromInt(int64(count)), at, nil, 1)
		if err != nil {
			return err
		}
		charge, err := failedBillingCharge(db, 1, money(decimal.NewFromInt(int64(count))), at)
		if err != nil {
			return err
		}
		for index := range count {
			if err = mutateBucket(db, bucket, "many-settle:"+sid(uint64(index+1)), "settle", dec("-1"), decimal.Zero, row{"charge_id": charge["id"]}); err != nil {
				return err
			}
			if index > 0 {
				from := at.Add(time.Duration(index) * time.Second)
				if _, err = insert(db, "usage_charges", row{"account_id": 1, "session_id": 1, "from_at": from, "to_at": from.Add(time.Second), "expected_pt": "0.000000", "settled_pt": "0.000000", "status": "settled", "settled_at": from.Add(time.Second), "calculation_snapshot": `{}`}); err != nil {
					return err
				}
			}
		}
		return db.Table("usage_charges").Where("id=?", charge["id"]).Updates(row{"settled_pt": charge["expected_pt"], "status": "settled", "settled_at": at.Add(time.Second)}).Error
	}); err != nil {
		t.Fatal(err)
	}
	failedAt := failedBillingTask(t, f)
	rollback := errors.New("crash before commit")
	if err := f.db.Transaction(func(db *gorm.DB) error {
		if _, err := account(db, 2); err != nil {
			return err
		}
		if err := CancelFailedCreation(f.ctx, db, row{"id": 1}, failedAt); err != nil {
			return err
		}
		return rollback
	}); !errors.Is(err, rollback) {
		t.Fatalf("rollback cleanup: %v", err)
	}
	if f.count(t, "credit_ledger", "entry_type='refund'") != 0 || f.count(t, "usage_charges", "status='cancelled'") != 0 {
		t.Fatal("rolled-back cleanup escaped its transaction")
	}
	if err := cancelFailedBillingChunk(f, failedAt); err != nil {
		t.Fatal(err)
	}
	if f.count(t, "credit_ledger", "entry_type='refund'") != settlementChunkSize || f.count(t, "usage_charges", "status='cancelled'") != settlementChunkSize {
		t.Fatal("first financial cleanup transaction was not bounded")
	}
	if pending, err := FailedCreationBillingPending(f.ctx, f.db, row{"id": 1}); err != nil || !pending {
		t.Fatalf("partially refunded cleanup reported complete: %v %v", pending, err)
	}
	for range 3 {
		before := f.count(t, "credit_ledger", "entry_type='refund'")
		if err := cancelFailedCreations(f.ctx, platform.New(f.db, platform.Config{})); err != nil {
			t.Fatal(err)
		}
		if got := f.count(t, "credit_ledger", "entry_type='refund'") - before; got > settlementChunkSize {
			t.Fatalf("restart refunded %d sources in one chunk", got)
		}
	}
	if pending, err := FailedCreationBillingPending(f.ctx, f.db, row{"id": 1}); err != nil || pending {
		t.Fatalf("restart did not finish cleanup: %v %v", pending, err)
	}
	if f.count(t, "credit_ledger", "entry_type='refund'") != int64(count) || f.count(t, "usage_charges", "status='cancelled'") != int64(count) {
		t.Fatal("some failed charge sources or unpaid charges were skipped")
	}
}

func TestMySQLFailedCreationConcurrentRefundsAreExactlyOnce(t *testing.T) {
	f := newAdjustmentMySQLFixture(t)
	at := now().Add(-time.Hour)
	if err := f.db.Transaction(func(db *gorm.DB) error {
		ac, err := account(db, 2)
		if err != nil {
			return err
		}
		if _, err = grant(db, ac, "admin", "concurrent-failed-source", dec("5"), at, nil, 1); err != nil {
			return err
		}
		charge, err := failedBillingCharge(db, 1, "5.000000", at)
		if err != nil {
			return err
		}
		return payCharge(db, ac, row{"id": 1}, charge, at)
	}); err != nil {
		t.Fatal(err)
	}
	failedAt := failedBillingTask(t, f)
	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			results <- cancelFailedBillingChunk(f, failedAt)
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if f.count(t, "credit_ledger", "entry_type='refund' AND amount_delta_pt=5") != 1 {
		t.Fatal("concurrent cleanup did not refund exactly once")
	}
}

func TestMySQLFailedCreationRejectsDeliveredOrUnmarkedInstances(t *testing.T) {
	for _, evidence := range []string{"unmarked", "successful_task", "verify_checkpoint", "delivery_snapshot", "wrong_task", "wrong_failure_time"} {
		t.Run(evidence, func(t *testing.T) {
			f := newAdjustmentMySQLFixture(t)
			failedAt := failedBillingTask(t, f)
			var err error
			switch evidence {
			case "unmarked":
				err = f.db.Exec("UPDATE instances SET creation_failed_at=NULL WHERE id=1").Error
			case "successful_task":
				err = f.db.Exec("UPDATE tasks SET status='succeeded' WHERE id=1").Error
			case "verify_checkpoint":
				err = f.db.Exec("INSERT INTO task_steps(task_id,step_key,attempt,status,parameters_redacted,finished_at) VALUES(1,'create.verify',1,'succeeded',JSON_OBJECT(),NOW(6))").Error
			case "delivery_snapshot":
				err = f.db.Exec("UPDATE instances SET incus_config_snapshot=JSON_SET(incus_config_snapshot,'$.creation_delivered',TRUE) WHERE id=1").Error
			case "wrong_task":
				err = f.db.Exec("UPDATE tasks SET task_type='instance.start' WHERE id=1").Error
			case "wrong_failure_time":
				failedAt = failedAt.Add(time.Second)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = cancelFailedBillingChunk(f, failedAt); err == nil {
				t.Fatal("unsafe failed-creation refund was accepted")
			}
			if f.count(t, "usage_sessions", "status='active'") != 1 || f.count(t, "credit_ledger", "1=1") != 0 {
				t.Fatal("rejected cleanup changed financial history")
			}
		})
	}
}

func TestMySQLFailedCreationAdminExemptLeavesFinancialHistoryUntouched(t *testing.T) {
	f := newAdjustmentMySQLFixture(t)
	f.addDebt(t, []string{"2.000000"})
	if err := f.db.Exec("UPDATE instances SET billing_mode='admin_exempt' WHERE id=1").Error; err != nil {
		t.Fatal(err)
	}
	failedAt := failedBillingTask(t, f)
	before := debtRow(t, f.db, "usage_charges", 1)
	if err := cancelFailedBillingChunk(f, failedAt); err != nil {
		t.Fatal(err)
	}
	assertDebtUnchanged(t, f.db, before)
	if f.count(t, "credit_ledger", "1=1") != 0 || f.count(t, "usage_sessions", "status='active'") != 1 {
		t.Fatal("admin-exempt cleanup mutated financial state")
	}
	if pending, err := FailedCreationBillingPending(f.ctx, f.db, debtRow(t, f.db, "instances", 1)); err != nil || pending {
		t.Fatalf("admin-exempt cleanup reported a bill: %v %v", pending, err)
	}
}

func TestMySQLFailedCreationMarkerOverridesEarlierTransactionSnapshot(t *testing.T) {
	f := newAdjustmentMySQLFixture(t)
	f.addDebt(t, []string{"1.000000"})
	stale := f.db.Begin()
	if stale.Error != nil {
		t.Fatal(stale.Error)
	}
	t.Cleanup(func() { stale.Rollback() })
	instance := debtRow(t, stale, "instances", 1)
	// Establish a repeatable-read snapshot before the failure transaction commits.
	failedBillingTask(t, f)
	if _, err := account(stale, 2); err != nil {
		t.Fatal(err)
	}
	if failed, err := instanceCreationFailed(stale, instance); err != nil || !failed {
		t.Fatalf("stale instance bypassed durable failure marker: %v %v", failed, err)
	}
	if failed, err := sessionCreationFailed(stale, row{"id": 1}); err != nil || !failed {
		t.Fatalf("stale session bypassed durable failure marker: %v %v", failed, err)
	}
	if debt, err := outstandingDebt(stale, 1); err != nil || !debt.IsZero() {
		t.Fatalf("stale snapshot restored failed debt: %s %v", money(debt), err)
	}
	if err := InstanceUsage(f.ctx, stale, instance, "running", nil); err != nil {
		t.Fatal(err)
	}
	if err := stale.Commit().Error; err != nil {
		t.Fatal(err)
	}
	if f.count(t, "usage_sessions", "1=1") != 2 || f.count(t, "credit_ledger", "1=1") != 0 {
		t.Fatal("pre-failure transaction snapshot reopened billing")
	}
}

func TestMySQLFailedCreationReleasesReservationBacklogInBoundedChunks(t *testing.T) {
	f := newAdjustmentMySQLFixture(t)
	count := settlementChunkSize + 3
	if err := f.db.Transaction(func(db *gorm.DB) error {
		ac, err := account(db, 2)
		if err != nil {
			return err
		}
		at := now()
		bucket, err := grant(db, ac, "admin", "failed-reservation-backlog", decimal.NewFromInt(int64(count)), at, nil, 1)
		if err != nil {
			return err
		}
		for index := range count {
			if err = failedBillingReservation(db, bucket, 2, "1.000000", "failed-held:"+sid(uint64(index+1)), at); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	failedAt := failedBillingTask(t, f)
	if err := cancelFailedBillingChunk(f, failedAt); err != nil {
		t.Fatal(err)
	}
	if got := f.count(t, "credit_reservations", "status='released'"); got != settlementChunkSize {
		t.Fatalf("one chunk released %d reservations", got)
	}
	if pending, err := FailedCreationBillingPending(f.ctx, f.db, row{"id": 1}); err != nil || !pending {
		t.Fatalf("unreleased reservation backlog not pending: %v %v", pending, err)
	}
	if err := cancelFailedCreations(f.ctx, platform.New(f.db, platform.Config{})); err != nil {
		t.Fatal(err)
	}
	if pending, err := FailedCreationBillingPending(f.ctx, f.db, row{"id": 1}); err != nil || pending {
		t.Fatalf("reservation backlog not completed after restart: %v %v", pending, err)
	}
	if f.count(t, "credit_reservations", "status='released'") != int64(count) || f.count(t, "credit_ledger", "entry_type='release'") != int64(count) {
		t.Fatal("reservation backlog lost its release audit trail")
	}
}
