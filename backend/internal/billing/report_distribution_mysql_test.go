package billing

import (
	"database/sql"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"101cloud/backend/internal/platform"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

func assertDistributionConservation(t *testing.T, out billingDistributionDTO) {
	t.Helper()
	sum := decimal.Zero
	for _, i := range out.Instances {
		if !dec(i.RecordedPT).Add(dec(i.EstimatedPT)).Equal(dec(i.ConsumedPT)) {
			t.Fatalf("instance split: %+v", i)
		}
		sum = sum.Add(dec(i.ConsumedPT))
	}
	b := out.Breakdown
	if !sum.Equal(dec(out.ConsumedPT)) || !dec(out.RecordedPT).Add(dec(out.EstimatedPT)).Equal(sum) || !dec(b.CPUPT).Add(dec(b.MemoryPT)).Add(dec(b.DiskPT)).Add(dec(b.GPUPT)).Equal(sum) {
		t.Fatalf("distribution conservation: %+v", out)
	}
}

func TestBillingDistributionStrictParams(t *testing.T) {
	for _, query := range []string{"", "period=", "period=all", "period=today&period=week", "period=today&instance_id=1", "period=today&user_id=2", "period=today&account_id=1", "period=today&search=x", "period=today&page=1", "period=today&page_size=20", "period=today&limit=20", "period=today&x=%zz", "period=today;week"} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("GET", "/?"+query, nil)
		if _, err := billingDistributionParams(c); err == nil {
			t.Fatalf("accepted %q", query)
		}
	}
	for _, p := range []string{"today", "week", "month", "year"} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("GET", "/?period="+p, nil)
		if got, err := billingDistributionParams(c); err != nil || got != p {
			t.Fatalf("period %s: %s %v", p, got, err)
		}
	}
}

func TestMySQLBillingDistributionUnpagedCohortAndConservation(t *testing.T) {
	f := reportFixture(t)
	at := now()
	if err := f.db.Table("instances").Where("id=1").Updates(row{"observed_state": "deleted", "desired_state": "deleted", "deleted_at": at}).Error; err != nil {
		t.Fatal(err)
	}
	reportCharge(t, f, 1, at.Add(-2*time.Second), at.Add(-time.Second), "0.000001", "insufficient")
	for id := uint64(2); id <= 29; id++ {
		i := debtRow(t, f.db, "instances", 1)
		i["id"], i["incus_name"], i["logical_name"] = id, "distribution-"+sid(id), "duplicate name"
		i["deleted_at"], i["observed_state"], i["desired_state"] = nil, "running", "stopped"
		if id == 27 {
			i["observed_state"], i["desired_state"] = "stopped", "running"
		}
		if id == 28 {
			i["user_id"] = 1
		}
		if id == 29 {
			i["creation_failed_at"] = at
		}
		if err := f.db.Table("instances").Create(&i).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{"today", "week", "month", "year"} {
		var out billingDistributionDTO
		err := f.db.Transaction(func(db *gorm.DB) error {
			ac, err := find(db, "credit_accounts", "id=1")
			if err != nil {
				return err
			}
			live, err := loadBillingUsage(db, ac, at, false)
			if err != nil {
				return err
			}
			out, err = billingDistribution(db, ac, at, live, p)
			if err != nil {
				return err
			}
			again, err := billingDistribution(db, ac, at, live, p)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(out, again) {
				t.Fatal("snapshot changed")
			}
			if err := db.Exec("UPDATE credit_accounts SET revision=revision+1 WHERE id=1").Error; err == nil {
				t.Fatal("read-only snapshot allowed write")
			}
			return nil
		}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(out.Instances) != 26 || out.Instances[0].InstanceID != "1" || !out.Instances[0].Deleted || out.Instances[1].InstanceID != "26" || out.ConsumedPT != "0.000001" || out.Breakdown.CPUPT != "0.000001" || out.Breakdown.MemoryPT != "0.000000" {
			t.Fatalf("%s cohort/micro rounding: %+v", p, out)
		}
		if !out.From.Equal(billingPeriod(at, p).From) || !out.To.Equal(at) {
			t.Fatalf("window: %+v", out)
		}
		assertDistributionConservation(t, out)
		var routed billingDistributionDTO
		reportCall(t, f, "/billing/distribution?period="+p, 200, &routed)
		if len(routed.Instances) != 26 {
			t.Fatalf("route cohort: %+v", routed)
		}
	}
	failedBillingTask(t, f)
	var failed billingDistributionDTO
	reportCall(t, f, "/billing/distribution?period=year", 200, &failed)
	if len(failed.Instances) != 25 || failed.ConsumedPT != "0.000000" {
		t.Fatalf("failure retained positive historical cost: %+v", failed)
	}
	assertDistributionConservation(t, failed)
	for _, query := range []string{"", "period=today&period=week", "period=year&instance_id=1", "period=year&account_id=1", "period=year&limit=20"} {
		reportCall(t, f, "/billing/distribution?"+query, 422, nil)
	}
	// An authenticated identity without an explicit wallet must not inherit
	// another member's cohort or receive a fabricated empty wallet.
	r := gin.New()
	r.Use(platform.RequestMiddleware())
	member := r.Group("/api/v1", func(c *gin.Context) { c.Set("user", platform.User{ID: 999999, Role: "user"}); c.Next() })
	registerBillingDistribution(platform.New(f.db, platform.Config{}), member)
	f.router = r
	reportCall(t, f, "/billing/distribution?period=today", 409, nil)
}

func TestMySQLBillingDistributionLateTailParticipation(t *testing.T) {
	at := reportLiveClock()
	f := reportLiveMySQLFixture(t, at)
	if err := f.db.Table("usage_sessions").Where("id=2").Update("last_settled_at", at.Add(-2*time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	// Fresh stop observation bounds a previously running tail. Historical usage
	// must participate even though this instance is not currently running.
	if err := f.db.Table("instances").Where("id=1").Updates(row{"observed_state": "stopped", "observed_at": at.Add(-time.Second)}).Error; err != nil {
		t.Fatal(err)
	}
	ac := debtRow(t, f.db, "credit_accounts", 1)
	live, err := loadBillingUsage(f.db, ac, at, false)
	if err != nil {
		t.Fatal(err)
	}
	out, err := billingDistribution(f.db, ac, at, live, "today")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Instances) != 1 || out.RecordedPT != "0.000000" || out.EstimatedPT != "1.000000" {
		t.Fatalf("late stopped tail omitted: %+v", out)
	}
	assertDistributionConservation(t, out)
}

func TestMySQLBillingDistributionCalendarClipping(t *testing.T) {
	f := reportFixture(t)
	at := time.Date(2026, 1, 1, 2, 0, 0, 0, zone)
	from := at.Add(-3 * time.Hour)
	if err := f.db.Exec("UPDATE usage_sessions SET started_at=?,last_settled_at=?,ended_at=? WHERE id=1", from, at, at).Error; err != nil {
		t.Fatal(err)
	}
	reportCharge(t, f, 1, from, at, "6.000000", "pending")
	for _, p := range []string{"today", "week", "month", "year"} {
		out, err := billingDistribution(f.db, row{"id": 1, "user_id": 2}, at, billingLive{}, p)
		if err != nil {
			t.Fatal(err)
		}
		want := "4.000000"
		if p == "week" {
			want = "6.000000"
		}
		if out.ConsumedPT != want {
			t.Fatalf("%s boundary: got %s want %s", p, out.ConsumedPT, want)
		}
		assertDistributionConservation(t, out)
	}
}
