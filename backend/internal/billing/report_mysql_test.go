package billing

import (
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"101cloud/backend/internal/platform"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

func reportFixture(t *testing.T) *adjustmentMySQLFixture {
	t.Helper()
	f := newAdjustmentMySQLFixture(t)
	// No active or held workload: these tests isolate historical SQL semantics.
	at := now().Add(-72 * time.Hour)
	if err := f.db.Exec("UPDATE usage_sessions SET status='ended',ended_at=?,last_settled_at=? WHERE id=2", at, at).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Exec(`UPDATE usage_sessions SET config_snapshot=JSON_OBJECT('vcpus',1,'memory_bytes','1073741824'),cpu_hourly_pt=1,memory_gib_hourly_pt=1`).Error; err != nil {
		t.Fatal(err)
	}
	a := platform.New(f.db, platform.Config{})
	r := gin.New()
	r.Use(platform.RequestMiddleware())
	member := r.Group("/api/v1", func(c *gin.Context) { c.Set("user", platform.User{ID: 2, Role: "user"}); c.Next() })
	Register(a, member, r.Group("/api/v1/admin", platform.Admin()))
	f.router = r
	return f
}
func reportCall(t *testing.T, f *adjustmentMySQLFixture, path string, want int, target any) {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1"+path, nil).WithContext(f.ctx)
	f.router.ServeHTTP(w, req)
	if w.Code != want {
		t.Fatalf("%s: status=%d body=%s", path, w.Code, w.Body.String())
	}
	if target != nil {
		var envelope struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(envelope.Data, target); err != nil {
			t.Fatal(err)
		}
	}
}
func reportCharge(t *testing.T, f *adjustmentMySQLFixture, session uint64, from, to time.Time, value, status string) {
	t.Helper()
	paid := "0.000000"
	var settledAt any
	if status == "settled" {
		paid, settledAt = value, to
	}
	if _, err := insert(f.db, "usage_charges", row{"session_id": session, "account_id": 1, "from_at": from, "to_at": to, "expected_pt": value, "settled_pt": paid, "status": status, "settled_at": settledAt, "calculation_snapshot": `{}`}); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLBillingReportsSQLOverlapConservationAndSnapshotReadOnly(t *testing.T) {
	f := reportFixture(t)
	midnight := time.Date(2026, 11, 9, 0, 0, 0, 0, zone)
	// One micro across three equal parts: independent rounding would lose it.
	reportCharge(t, f, 1, midnight.Add(-time.Second), midnight.Add(2*time.Second), "0.000001", "insufficient")
	reportCharge(t, f, 1, midnight.Add(3*time.Second), midnight.Add(4*time.Second), "99.000000", "cancelled")
	windows := []billingWindow{{midnight.Add(-time.Second), midnight}, {midnight, midnight.Add(time.Second)}, {midnight.Add(time.Second), midnight.Add(2 * time.Second)}}
	query, args := billingAggregateSQL(row{"id": 1, "user_id": 2}, windows, nil, 0, false)
	var totals []billingAggregate
	if err := f.db.Transaction(func(db *gorm.DB) error {
		if err := db.Raw(query, args...).Scan(&totals).Error; err != nil {
			return err
		}
		// A financial mutation must actually be rejected by the snapshot, not
		// merely absent from today's implementation.
		if err := db.Exec("UPDATE credit_accounts SET revision=revision+1 WHERE id=1").Error; err == nil {
			t.Fatal("report snapshot allowed financial write")
		}
		return nil
	}, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead}); err != nil {
		t.Fatal(err)
	}
	want := []string{"0.000000", "0.000001", "0.000000"}
	if len(totals) != 3 {
		t.Fatalf("missing overlap buckets: %+v", totals)
	}
	got := make([]string, 3)
	var sum decimal.Decimal
	for _, v := range totals {
		got[v.WindowID] = money(v.consumed())
		sum = sum.Add(v.consumed())
		if !dec(v.CPU).Add(dec(v.Memory)).Add(dec(v.Disk)).Add(dec(v.GPU)).Equal(v.consumed()) {
			t.Fatalf("component drift: %+v", v)
		}
	}
	if !reflect.DeepEqual(got, want) || money(sum) != "0.000001" {
		t.Fatalf("nonconserving splits: %v", got)
	}
	query, args = billingAggregateSQL(row{"id": 1, "user_id": 2}, []billingWindow{{midnight.Add(-time.Hour), midnight.Add(time.Hour)}}, nil, 0, false)
	var whole []billingAggregate
	if err := f.db.Raw(query, args...).Scan(&whole).Error; err != nil {
		t.Fatal(err)
	}
	if len(whole) != 1 || money(whole[0].consumed()) != "0.000001" {
		t.Fatalf("cancelled included or whole differs: %+v", whole)
	}
	// The same cost disappears immediately on failure, before refund chunks.
	failedBillingTask(t, f)
	totals = nil
	if err := f.db.Raw(query, args...).Scan(&totals).Error; err != nil {
		t.Fatal(err)
	}
	if len(totals) != 0 {
		t.Fatalf("failed creation remained billable: %+v", totals)
	}
}

func TestMySQLBillingReportRoutesTombstonesSearchPaginationAndNoWrites(t *testing.T) {
	f := reportFixture(t)
	at := now()
	from := billingPeriod(at, "year").From
	reportCharge(t, f, 1, from, from.Add(time.Microsecond), "7.000000", "insufficient")
	for _, id := range []uint64{2, 3} {
		i := debtRow(t, f.db, "instances", 1)
		i["id"], i["incus_name"], i["logical_name"] = id, "report-instance-"+sid(id), "Report % literal "+sid(id)
		if id == 2 {
			i["deleted_at"], i["desired_state"], i["observed_state"] = at, "deleted", "deleted"
		}
		if err := f.db.Table("instances").Create(&i).Error; err != nil {
			t.Fatal(err)
		}
	}
	foreign := debtRow(t, f.db, "instances", 1)
	foreign["id"], foreign["user_id"], foreign["incus_name"] = 4, 1, "report-foreign-instance"
	if err := f.db.Table("instances").Create(&foreign).Error; err != nil {
		t.Fatal(err)
	}
	before := map[string]int64{}
	for _, table := range []string{"usage_charges", "credit_ledger", "credit_buckets", "credit_reservations", "tasks"} {
		before[table] = f.count(t, table, "1=1")
	}
	var list billingInstancesDTO
	reportCall(t, f, "/billing/instances?period=year&page_size=1", 200, &list)
	if list.Total != 3 || len(list.Items) != 1 || list.Items[0].InstanceID != "1" || list.Items[0].RecordedPT != "7.000000" || list.Items[0].EstimatedPT != "0.000000" {
		t.Fatalf("list sorting/SQL scan failed: %+v", list)
	}
	if list.Items[0].Breakdown.CPUPT != "3.500000" || list.Items[0].Breakdown.MemoryPT != "3.500000" || list.Items[0].Breakdown.DiskPT != "0.000000" || list.Items[0].Breakdown.GPUPT != "0.000000" {
		t.Fatalf("snapshot component split: %+v", list.Items[0])
	}
	reportCall(t, f, "/billing/instances?period=year&page_size=1&page=2", 200, &list)
	if len(list.Items) != 1 || list.Items[0].InstanceID != "3" {
		t.Fatalf("ID tiebreak failed: %+v", list)
	}
	reportCall(t, f, "/billing/instances?period=year&search=%25", 200, &list)
	if list.Total != 2 {
		t.Fatalf("literal search failed: %+v", list)
	}
	reportCall(t, f, "/billing/instances?period=year&instance_id=2", 200, &list)
	if list.Total != 1 || len(list.Items) != 1 || !list.Items[0].Deleted || list.Items[0].Name != "Report % literal 2" {
		t.Fatalf("tombstone missing: %+v", list)
	}
	var overview billingOverviewDTO
	reportCall(t, f, "/billing/overview", 200, &overview)
	if overview.Periods["year"].ConsumedPT != "7.000000" {
		t.Fatalf("unpaid consumption excluded: %+v", overview)
	}
	var trend billingTrendDTO
	reportCall(t, f, "/billing/trend?period=year", 200, &trend)
	if len(trend.Items) != 12 || trend.Granularity != "month" {
		t.Fatalf("axis missing: %+v", trend)
	}
	var sum decimal.Decimal
	for _, item := range trend.Items {
		if !item.From.Before(trend.AsOf) {
			if item.ConsumedPT != nil || item.RecordedPT != nil || item.EstimatedPT != nil {
				t.Fatal("future bucket fabricated zero")
			}
			continue
		}
		if item.ConsumedPT == nil {
			t.Fatal("elapsed bucket unknown")
		}
		sum = sum.Add(dec(*item.ConsumedPT))
		if item.To.After(trend.AsOf) && !item.Partial {
			t.Fatal("current bucket not partial")
		}
	}
	if money(sum) != overview.Periods["year"].ConsumedPT {
		t.Fatal("trend/overview mismatch")
	}
	for _, path := range []string{"/billing/overview?instance_id=1", "/billing/trend?period=year&period=month", "/billing/instances?period=year&page_size=101", "/billing/instances?period=year&user_id=1"} {
		reportCall(t, f, path, 422, nil)
	}
	reportCall(t, f, "/billing/trend?period=year&instance_id=999999", 404, nil)
	reportCall(t, f, "/billing/trend?period=year&instance_id=4", 404, nil)
	reportCall(t, f, "/billing/instances?period=year&instance_id=4", 404, nil)
	for table, count := range before {
		if f.count(t, table, "1=1") != count {
			t.Fatalf("GET wrote %s", table)
		}
	}
}

func TestMySQLBillingReportsCrossYearOverlap(t *testing.T) {
	f := reportFixture(t)
	at := time.Date(2026, 1, 1, 2, 0, 0, 0, zone)
	from := at.Add(-3 * time.Hour)
	if err := f.db.Exec("UPDATE usage_sessions SET started_at=?,last_settled_at=?,ended_at=? WHERE id=1", from, at, at).Error; err != nil {
		t.Fatal(err)
	}
	reportCharge(t, f, 1, from, at, "6.000000", "pending")
	overview, err := billingOverview(f.db, row{"id": 1, "user_id": 2}, at, billingLive{})
	if err != nil {
		t.Fatal(err)
	}
	for _, period := range []string{"today", "month", "year"} {
		if got := overview.Periods[period].ConsumedPT; got != "4.000000" {
			t.Fatalf("%s lost cross-year usage: %s", period, got)
		}
	}
	if got := overview.Periods["week"].ConsumedPT; got != "6.000000" {
		t.Fatalf("Monday-based week lost prior-year usage: %s", got)
	}
}
