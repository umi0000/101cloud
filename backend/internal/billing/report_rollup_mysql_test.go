package billing

import (
	"database/sql"
	"errors"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"101cloud/backend/internal/platform"
	"gorm.io/gorm"
)

func reportRollupTotals(t *testing.T, db *gorm.DB, windows []billingWindow, instance uint64) []string {
	t.Helper()
	query, args := billingAggregateSQL(row{"id": 1, "user_id": 2}, windows, nil, instance, true)
	var values []billingAggregate
	if err := db.Raw(query, args...).Scan(&values).Error; err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(values))
	for _, v := range values {
		if dec(v.Recorded).IsZero() && dec(v.CPU).IsZero() && dec(v.Memory).IsZero() && dec(v.Disk).IsZero() && dec(v.GPU).IsZero() {
			continue
		}
		if !dec(v.CPU).Add(dec(v.Memory)).Add(dec(v.Disk)).Add(dec(v.GPU)).Equal(dec(v.Recorded)) {
			t.Fatalf("component drift: %+v", v)
		}
		out = append(out, sid(v.WindowID)+":"+sid(v.InstanceID)+":"+money(dec(v.Recorded))+":"+money(dec(v.CPU))+":"+money(dec(v.Memory))+":"+money(dec(v.Disk))+":"+money(dec(v.GPU)))
	}
	sort.Strings(out)
	return out
}

func reportRollupOriginals(t *testing.T, db *gorm.DB) []row {
	t.Helper()
	var out []row
	if err := db.Table("usage_charges").Select("id,session_id,account_id,from_at,to_at,expected_pt,settled_pt,status,calculation_snapshot,settled_at,revision,created_at,updated_at").Order("id").Find(&out).Error; err != nil {
		t.Fatal(err)
	}
	return out
}

func reportRollupWarm(t *testing.T, f *adjustmentMySQLFixture) {
	t.Helper()
	if err := WarmBillingReports(f.ctx, f.db); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLBillingRollupExactHybridBoundariesAndReadOnly(t *testing.T) {
	f := reportFixture(t)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, zone)
	for _, statement := range []string{
		`INSERT INTO gpu_price_versions(id,code,version,name,gpu_model,access_mode,gpu_hourly_pt,status,effective_at,created_by_user_id) VALUES(1,'report-gpu','1','Report GPU','Test GPU','shared',1,'active','2000-01-01',1)`,
		`INSERT INTO node_gpus(id,node_id,uuid,model,memory_mib,max_allocations,status) VALUES(1,1,'GPU-report-fixture','Test GPU',1024,1,'available')`,
		`INSERT INTO gpu_allocations(id,instance_id,user_id,node_id,gpu_id,gpu_price_version_id,gpu_hourly_pt,status,reserved_at,released_at) VALUES(1,1,2,1,1,1,1,'released','2025-12-31 20:00:00','2026-01-03 00:00:00')`,
	} {
		if err := f.db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	for id, kind := range map[uint64]string{3: "storage", 4: "gpu"} {
		s := debtRow(t, f.db, "usage_sessions", 1)
		s["id"], s["charge_kind"] = id, kind
		delete(s, "active_instance_id")
		delete(s, "allocation_key")
		s["cpu_hourly_pt"], s["memory_gib_hourly_pt"] = "0", "0"
		s["started_at"], s["last_settled_at"], s["ended_at"] = start.Add(-2*time.Hour), start.Add(26*time.Hour), start.Add(26*time.Hour)
		if kind == "storage" {
			s["disk_gib_hourly_pt"] = "1"
		} else {
			s["gpu_hourly_pt"], s["gpu_allocation_id"], s["gpu_price_version_id"] = "1", 1, 1
		}
		if err := f.db.Table("usage_sessions").Create(&s).Error; err != nil {
			t.Fatal(err)
		}
	}
	reportCharge(t, f, 1, start.Add(-time.Hour), start.Add(49*time.Hour), "1234567890123.123457", "pending")
	reportCharge(t, f, 1, start.Add(-time.Second), start.Add(2*time.Second), "0.000001", "insufficient")
	reportCharge(t, f, 1, start.Add(3*time.Hour), start.Add(6*time.Hour), "0.000001", "pending")
	reportCharge(t, f, 3, start.Add(27*time.Minute), start.Add(25*time.Hour+17*time.Minute), "7.000003", "settled")
	reportCharge(t, f, 4, start.Add(-2*time.Hour), start.Add(3*time.Hour), "3.000001", "pending")
	reportCharge(t, f, 1, start, start.Add(72*time.Hour), "999.000000", "cancelled")
	windows := []billingWindow{
		{start.AddDate(-1, 0, 0), start}, {start, start.AddDate(1, 0, 0)},
		{start, start.Add(24 * time.Hour)}, {start.Add(24 * time.Hour), start.Add(48 * time.Hour)},
		{start.Add(-time.Second), start}, {start, start.Add(time.Second)}, {start.Add(time.Second), start.Add(2 * time.Second)},
		{start.Add(17*time.Minute + time.Microsecond), start.Add(29*time.Hour + 123*time.Microsecond)},
		{start.Add(3 * time.Hour), start.Add(4 * time.Hour)}, {start.Add(4 * time.Hour), start.Add(5 * time.Hour)}, {start.Add(5 * time.Hour), start.Add(6 * time.Hour)},
		{start.Add(50 * time.Hour), start.Add(60 * time.Hour)},
	}
	before := reportRollupTotals(t, f.db, windows, 0)
	originals := reportRollupOriginals(t, f.db)
	reportRollupWarm(t, f)
	if got := reportRollupTotals(t, f.db, windows, 0); !reflect.DeepEqual(got, before) {
		t.Fatalf("warm changed cumulative allocation:\n%v\n%v", before, got)
	}
	if !reflect.DeepEqual(originals, reportRollupOriginals(t, f.db)) {
		t.Fatal("backfill changed original financial source fields")
	}
	if f.count(t, "usage_charges", "report_rollup_complete=FALSE") != 0 {
		t.Fatal("backfill did not finish")
	}
	// A tombstone still contributes, and filtering stays instance-scoped.
	if err := f.db.Exec("UPDATE instances SET deleted_at=?,desired_state='deleted',observed_state='deleted' WHERE id=1", start.AddDate(0, 2, 0)).Error; err != nil {
		t.Fatal(err)
	}
	if got := reportRollupTotals(t, f.db, windows, 1); !reflect.DeepEqual(got, before) {
		t.Fatalf("tombstone disappeared: %v", got)
	}
	// GET works throughout a backfill: direct fixture inserts remain the exact
	// unprocessed complement, including late historical charges.
	reportCharge(t, f, 3, start.Add(time.Hour), start.Add(2*time.Hour), "0.000007", "insufficient")
	hybrid := reportRollupTotals(t, f.db, windows, 0)
	reportRollupWarm(t, f)
	if got := reportRollupTotals(t, f.db, windows, 0); !reflect.DeepEqual(got, hybrid) {
		t.Fatalf("late source missing/doubled: %v vs %v", hybrid, got)
	}
	// Missing metadata is conservative, not an empty or overcounted dashboard.
	if err := f.db.Exec("DELETE FROM billing_report_state WHERE account_id=1").Error; err != nil {
		t.Fatal(err)
	}
	if got := reportRollupTotals(t, f.db, windows, 0); !reflect.DeepEqual(got, hybrid) {
		t.Fatal("metadata absence lost exact fallback")
	}
	originals = reportRollupOriginals(t, f.db)
	if err := f.db.Transaction(func(db *gorm.DB) error {
		if got := reportRollupTotals(t, db, windows, 0); !reflect.DeepEqual(got, hybrid) {
			t.Fatal("readonly hybrid mismatch")
		}
		if err := db.Exec("UPDATE billing_report_hours SET recorded=recorded+1 WHERE account_id=1").Error; err == nil {
			t.Fatal("read-only report transaction accepted a derived write")
		}
		return nil
	}, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(originals, reportRollupOriginals(t, f.db)) {
		t.Fatal("query rewrote sources")
	}
}

func TestMySQLBillingRollupPartialHourFutureAndTinyFees(t *testing.T) {
	f := reportFixture(t)
	start := time.Date(2026, 11, 9, 10, 0, 0, 0, zone)
	windows := []billingWindow{{start, start.Add(20 * time.Minute)}, {start, start.Add(40 * time.Minute)}, {start, start.Add(time.Hour)}, {start.Add(11 * time.Minute), start.Add(47 * time.Minute)}}
	reportCharge(t, f, 1, start, start.Add(10*time.Minute), "0.000001", "pending")
	reportCharge(t, f, 1, start.Add(10*time.Minute), start.Add(30*time.Minute), "0.000003", "pending")
	before := reportRollupTotals(t, f.db, windows, 0)
	reportRollupWarm(t, f)
	if got := reportRollupTotals(t, f.db, windows, 0); !reflect.DeepEqual(got, before) {
		t.Fatalf("future partial hour mismatch: %v %v", before, got)
	}
	// First two windows respectively require raw fallback and allow the safe
	// partial-hour summary. Both agree with the raw consumer result.
	if got := reportRollupTotals(t, f.db, windows[1:2], 0); len(got) != 1 || got[0] != "0:1:0.000004:0.000003:0.000001:0.000000:0.000000" {
		t.Fatalf("tiny cumulative CPU allocation: %v", got)
	}
}

func TestMySQLBillingRollupChunkRollbackConcurrentResumeAndLateLowerID(t *testing.T) {
	f := reportFixture(t)
	start := time.Date(2026, 5, 1, 0, 0, 0, 0, zone)
	charges := make([]row, 0, billingReportChunkSize+7)
	for index := range billingReportChunkSize + 7 {
		from := start.Add(time.Duration(index) * time.Second)
		charges = append(charges, row{"id": index + 100, "session_id": 1, "account_id": 1, "from_at": from, "to_at": from.Add(time.Second), "expected_pt": "0.000001", "settled_pt": "0.000000", "status": "pending", "calculation_snapshot": "{}"})
	}
	if err := f.db.Table("usage_charges").CreateInBatches(charges, 250).Error; err != nil {
		t.Fatal(err)
	}
	windows := []billingWindow{{start, start.Add(24 * time.Hour)}}
	before := reportRollupTotals(t, f.db, windows, 0)
	originals := reportRollupOriginals(t, f.db)
	rollback := errors.New("abort report chunk")
	if err := f.db.Transaction(func(db *gorm.DB) error {
		if _, err := platform.Get(db, "credit_accounts", 1, true); err != nil {
			return err
		}
		if err := warmBillingReportChunk(db, 1); err != nil {
			return err
		}
		return rollback
	}); !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	if f.count(t, "billing_report_hours", "1=1") != 0 || f.count(t, "billing_report_days", "1=1") != 0 || f.count(t, "billing_report_state", "1=1") != 0 || f.count(t, "usage_charges", "report_rollup_complete=TRUE") != 0 {
		t.Fatal("derived/source state escaped rollback")
	}
	if err := f.db.Transaction(func(db *gorm.DB) error {
		if _, err := platform.Get(db, "credit_accounts", 1, true); err != nil {
			return err
		}
		return warmBillingReportChunk(db, 1)
	}); err != nil {
		t.Fatal(err)
	}
	if got := reportRollupTotals(t, f.db, windows, 0); !reflect.DeepEqual(got, before) {
		t.Fatal("partial backfill omitted or duplicated complement")
	}
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- WarmBillingReports(f.ctx, f.db) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := reportRollupTotals(t, f.db, windows, 0); !reflect.DeepEqual(got, before) {
		t.Fatal("concurrent resume changed totals")
	}
	if !reflect.DeepEqual(originals, reportRollupOriginals(t, f.db)) {
		t.Fatal("resume changed financial source")
	}
	// A lower-ID source arriving after complete higher IDs must still be found.
	if _, err := insert(f.db, "usage_charges", row{"id": 3, "session_id": 1, "account_id": 1, "from_at": start.Add(30 * time.Minute), "to_at": start.Add(time.Hour), "expected_pt": "0.000009", "settled_pt": "0", "status": "pending", "calculation_snapshot": "{}"}); err != nil {
		t.Fatal(err)
	}
	hybrid := reportRollupTotals(t, f.db, windows, 0)
	reportRollupWarm(t, f)
	reportRollupWarm(t, f)
	if got := reportRollupTotals(t, f.db, windows, 0); !reflect.DeepEqual(got, hybrid) || reflect.DeepEqual(got, before) {
		t.Fatal("late lower ID skipped or retry doubled contribution")
	}
}

func TestMySQLBillingRollupSettlementFailureCancellationRollbackRetry(t *testing.T) {
	f := reportFixture(t)
	at := time.Date(2026, 8, 1, 11, 0, 0, 0, zone)
	if err := f.db.Exec("UPDATE usage_sessions SET started_at=?,last_settled_at=?,ended_at=NULL,status='active',metered_microseconds=0,cumulative_expected_pt=0 WHERE id=1", at, at).Error; err != nil {
		t.Fatal(err)
	}
	rollback := errors.New("abort settlement")
	settle := func(db *gorm.DB) error {
		ac, err := account(db, 2)
		if err != nil {
			return err
		}
		p, err := accountPolicy(db, ac)
		if err != nil {
			return err
		}
		s, err := platform.Get(db, "usage_sessions", 1, true)
		if err != nil {
			return err
		}
		return settleSession(db, ac, p, s, at.Add(time.Hour))
	}
	if err := f.db.Transaction(func(db *gorm.DB) error {
		if err := settle(db); err != nil {
			return err
		}
		return rollback
	}); !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	if f.count(t, "usage_charges", "1=1") != 0 || f.count(t, "billing_report_hours", "1=1") != 0 || f.count(t, "billing_report_state", "1=1") != 0 {
		t.Fatal("settlement/source rollup escaped financial rollback")
	}
	if err := f.db.Transaction(settle); err != nil {
		t.Fatal(err)
	}
	windows := []billingWindow{{at, at.Add(time.Hour)}}
	before := reportRollupTotals(t, f.db, windows, 0)
	if len(before) != 1 || before[0] != "0:1:2.000000:1.000000:1.000000:0.000000:0.000000" {
		t.Fatalf("real settlement did not materialize expected consumption: %v", before)
	}
	reportRollupWarm(t, f)
	if got := reportRollupTotals(t, f.db, windows, 0); !reflect.DeepEqual(got, before) {
		t.Fatal("warm doubled transactional insert")
	}
	failedAt := failedBillingTask(t, f)
	if got := reportRollupTotals(t, f.db, windows, 0); len(got) != 0 {
		t.Fatal("failure marker did not immediately exclude materialized cost")
	}
	if err := f.db.Transaction(func(db *gorm.DB) error {
		if _, err := account(db, 2); err != nil {
			return err
		}
		if err := CancelFailedCreation(f.ctx, db, row{"id": 1}, failedAt); err != nil {
			return err
		}
		return rollback
	}); !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	if f.count(t, "usage_charges", "report_rollup_applied=TRUE AND status<>'cancelled'") != 1 {
		t.Fatal("cancellation rollback lost applied source state")
	}
	if err := cancelFailedBillingChunk(f, failedAt); err != nil {
		t.Fatal(err)
	}
	if err := cancelFailedBillingChunk(f, failedAt); err != nil {
		t.Fatal(err)
	}
	var sum string
	if err := f.db.Raw("SELECT CAST(COALESCE(SUM(recorded),0) AS DECIMAL(40,6)) FROM billing_report_hours WHERE account_id=1").Scan(&sum).Error; err != nil {
		t.Fatal(err)
	}
	if sum != "0.000000" || f.count(t, "usage_charges", "report_rollup_applied=TRUE") != 0 {
		t.Fatal("cancellation did not reverse exactly once")
	}
	reportRollupWarm(t, f)
	if got := reportRollupTotals(t, f.db, windows, 0); len(got) != 0 {
		t.Fatal("retry restored failed cost")
	}
}

func TestMySQLBillingRollupFailedBeforeWarmDoesNotSubtractUnappliedCost(t *testing.T) {
	f := reportFixture(t)
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, zone)
	reportCharge(t, f, 1, at, at.Add(time.Hour), "5.000001", "pending")
	failedAt := failedBillingTask(t, f)
	reportRollupWarm(t, f)
	if got := reportRollupTotals(t, f.db, []billingWindow{{at, at.Add(24 * time.Hour)}}, 0); len(got) != 0 {
		t.Fatal("warm included already-failed source")
	}
	if err := cancelFailedBillingChunk(f, failedAt); err != nil {
		t.Fatal(err)
	}
	if f.count(t, "billing_report_hours", "recorded<>0") != 0 || f.count(t, "billing_report_days", "recorded<>0") != 0 {
		t.Fatal("cancellation subtracted a never-applied source")
	}
}

func TestMySQLBillingRollupRawChargeDTORemainsUnchanged(t *testing.T) {
	f := reportFixture(t)
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, zone)
	reportCharge(t, f, 1, at, at.Add(time.Hour), "1.234567", "pending")
	var before, after struct {
		Items []map[string]any `json:"items"`
	}
	reportCall(t, f, "/usage/charges", 200, &before)
	reportRollupWarm(t, f)
	reportCall(t, f, "/usage/charges", 200, &after)
	if len(before.Items) != 1 || !reflect.DeepEqual(before, after) {
		t.Fatalf("materialization changed raw DTO: %v vs %v", before, after)
	}
	for _, item := range after.Items {
		if _, ok := item["report_rollup_complete"]; ok {
			t.Fatal("internal complete flag leaked")
		}
		if _, ok := item["report_rollup_applied"]; ok {
			t.Fatal("internal applied flag leaked")
		}
	}
}

func TestMySQLBillingRollupFailureExcludesAllChunksBeforeCancellationFinishes(t *testing.T) {
	f := reportFixture(t)
	at := time.Date(2026, 7, 1, 0, 0, 0, 0, zone)
	charges := make([]row, 0, settlementChunkSize+5)
	for index := range settlementChunkSize + 5 {
		from := at.Add(time.Duration(index) * time.Second)
		charges = append(charges, row{"session_id": 1, "account_id": 1, "from_at": from, "to_at": from.Add(time.Second), "expected_pt": "0.000001", "settled_pt": "0.000000", "status": "pending", "calculation_snapshot": "{}"})
	}
	if err := f.db.Table("usage_charges").CreateInBatches(charges, 250).Error; err != nil {
		t.Fatal(err)
	}
	reportRollupWarm(t, f)
	windows := []billingWindow{{at, at.Add(24 * time.Hour)}}
	if got := reportRollupTotals(t, f.db, windows, 0); len(got) != 1 {
		t.Fatal("materialized chunk fixture missing")
	}
	failedAt := failedBillingTask(t, f)
	if got := reportRollupTotals(t, f.db, windows, 0); len(got) != 0 {
		t.Fatal("failure marker left pre-cancellation materialized costs")
	}
	if err := cancelFailedBillingChunk(f, failedAt); err != nil {
		t.Fatal(err)
	}
	if f.count(t, "usage_charges", "status<>'cancelled'") != 5 {
		t.Fatal("cleanup did not retain bounded pending complement")
	}
	if got := reportRollupTotals(t, f.db, windows, 0); len(got) != 0 {
		t.Fatal("first cancellation chunk exposed remaining materialized costs")
	}
	if err := cancelFailedBillingChunk(f, failedAt); err != nil {
		t.Fatal(err)
	}
	if err := cancelFailedBillingChunk(f, failedAt); err != nil {
		t.Fatal(err)
	}
	if f.count(t, "billing_report_hours", "recorded<>0") != 0 || f.count(t, "billing_report_days", "recorded<>0") != 0 {
		t.Fatal("retry left or doubled reversed materialized amounts")
	}
}

func TestMySQLBillingRollupCancellationUsesCurrentSourceAfterOlderSnapshot(t *testing.T) {
	f := reportFixture(t)
	at := time.Date(2026, 6, 1, 0, 0, 0, 0, zone)
	reportCharge(t, f, 1, at, at.Add(time.Hour), "0.000003", "pending")
	if err := f.db.Transaction(func(db *gorm.DB) error {
		// Establish a snapshot before a different transaction materializes the
		// source. A later financial locking read must not use stale flags.
		var count int64
		if err := db.Table("usage_charges").Count(&count).Error; err != nil {
			return err
		}
		reportRollupWarm(t, f)
		failedAt := failedBillingTask(t, f)
		if _, err := account(db, 2); err != nil {
			return err
		}
		return CancelFailedCreation(f.ctx, db, row{"id": 1}, failedAt)
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead}); err != nil {
		t.Fatal(err)
	}
	if f.count(t, "billing_report_hours", "recorded<>0") != 0 || f.count(t, "billing_report_days", "recorded<>0") != 0 {
		t.Fatal("older financial snapshot skipped materialized source reversal")
	}
	if got := reportRollupTotals(t, f.db, []billingWindow{{at, at.Add(time.Hour)}}, 0); len(got) != 0 {
		t.Fatal("cancelled source remained visible")
	}
}
