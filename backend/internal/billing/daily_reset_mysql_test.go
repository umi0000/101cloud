package billing

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"101cloud/backend/internal/platform"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

func resetProfile(t *testing.T, f *adjustmentMySQLFixture, allowance string) (row, time.Time) {
	t.Helper()
	p := validCreditPolicy()
	p["daily_allowance_pt"], p["daily_validity_seconds"] = allowance, 604800
	addAllowanceProfile(t, f, 2, 102, p)
	cycle, err := cycleAt(p, now())
	if err != nil {
		t.Fatal(err)
	}
	if err = f.db.Table("credit_accounts").Where("id=1").Updates(row{"credit_policy_version_id": 2, "created_at": cycle.AddDate(0, 0, -100)}).Error; err != nil {
		t.Fatal(err)
	}
	return p, cycle
}

func resetTransaction(t *testing.T, f *adjustmentMySQLFixture, fn func(*gorm.DB, row) error) {
	t.Helper()
	if err := f.db.Transaction(func(db *gorm.DB) error {
		ac, err := account(db, 2)
		if err != nil {
			return err
		}
		return fn(db, ac)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLDailyResetPreservesExtrasAndNeverRefills(t *testing.T) {
	f := newAdjustmentMySQLFixture(t)
	p, cycle := resetProfile(t, f, "100.000000")
	var extraIDs []uint64
	resetTransaction(t, f, func(db *gorm.DB, ac row) error {
		previous := cycle.AddDate(0, 0, -1)
		if err := grantDailyCycle(db, ac, p, previous, previous); err != nil {
			return err
		}
		b, err := find(db, "credit_buckets", "account_id=1 AND source_type='daily'")
		if err != nil {
			return err
		}
		if err = mutateBucket(db, b, "reset-spend", "settle", dec("-70"), decimal.Zero, nil); err != nil {
			return err
		}
		expiry := cycle.AddDate(0, 0, 7).In(zone)
		for _, source := range []string{"admin", "redemption"} {
			extra, err := grant(db, ac, source, "reset-extra:"+source, dec("25"), previous.In(zone), &expiry, 1)
			if err != nil {
				return err
			}
			extraIDs = append(extraIDs, platform.Uint(extra["id"]))
		}
		return nil
	})
	for range 2 {
		if err := dailyGrants(f.ctx, platform.New(f.db, platform.Config{})); err != nil {
			t.Fatal(err)
		}
	}
	left, held, err := balances(f.db, 1)
	if err != nil || !left.Equal(dec("150")) || !held.IsZero() {
		t.Fatalf("30 daily + 50 extra must reset to 100 + 50: %s/%s %v", left, held, err)
	}
	if f.count(t, "credit_ledger", "entry_type='expire' AND amount_delta_pt=-30") != 1 {
		t.Fatal("old daily remainder lacks exactly one expiration ledger entry")
	}
	for _, id := range extraIDs {
		b := debtRow(t, f.db, "credit_buckets", id)
		if !dec(b["remaining_pt"]).Equal(dec("25")) || !stamp(b["expires_at"]).Equal(cycle.AddDate(0, 0, 7)) {
			t.Fatalf("extra changed: %#v", b)
		}
	}
	resetTransaction(t, f, func(db *gorm.DB, ac row) error {
		b, err := find(db, "credit_buckets", "account_id=1 AND business_key=?", "daily:"+cycle.Format("2006-01-02"))
		if err != nil {
			return err
		}
		return mutateBucket(db, b, "consume-current", "settle", dec("-80"), decimal.Zero, nil)
	})
	ledger := f.count(t, "credit_ledger", "1=1")
	for range 2 {
		if err = dailyGrants(f.ctx, platform.New(f.db, platform.Config{})); err != nil {
			t.Fatal(err)
		}
		if err = NormalizeDailyBuckets(f.ctx, f.db); err != nil {
			t.Fatal(err)
		}
	}
	left, _, err = balances(f.db, 1)
	if err != nil || !left.Equal(dec("70")) || ledger != f.count(t, "credit_ledger", "1=1") {
		t.Fatalf("same-date retry/refill: %s %v", left, err)
	}
}

func TestMySQLDailyResetSkipsClosedPeriodsAndSerializesWriters(t *testing.T) {
	f := newAdjustmentMySQLFixture(t)
	p, cycle := resetProfile(t, f, "100.000000")
	if err := f.db.Table("credit_accounts").Where("id=1").Update("grant_cursor_at", cycle.AddDate(0, 0, -90)).Error; err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- dailyGrants(f.ctx, platform.New(f.db, platform.Config{}))
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if f.count(t, "credit_daily_grants", "account_id=1") != 1 || f.count(t, "credit_buckets", "account_id=1 AND granted_pt=100") != 1 {
		t.Fatal("missed periods accumulated or concurrent polls duplicated quota")
	}
	resetTransaction(t, f, func(db *gorm.DB, ac row) error {
		b, err := find(db, "credit_buckets", "account_id=1")
		if err != nil {
			return err
		}
		if err = mutateBucket(db, b, "spent-all", "settle", dec("-100"), decimal.Zero, nil); err != nil {
			return err
		}
		return grantDailyCycle(db, ac, p, cycle, cycle)
	})
	left, _, err := balances(f.db, 1)
	if err != nil || !left.IsZero() {
		t.Fatalf("consumed current cycle refilled: %s %v", left, err)
	}
}

func TestMySQLDailyResetZeroAllowanceConsumesCycle(t *testing.T) {
	f := newAdjustmentMySQLFixture(t)
	p, cycle := resetProfile(t, f, "0.000000")
	for range 2 {
		resetTransaction(t, f, func(db *gorm.DB, ac row) error { return grantDailyCycle(db, ac, p, cycle, cycle) })
	}
	if f.count(t, "credit_daily_grants", "account_id=1") != 1 || f.count(t, "credit_buckets", "account_id=1 AND granted_pt=0") != 1 || f.count(t, "credit_ledger", "account_id=1") != 0 {
		t.Fatal("zero allowance did not consume exactly one cycle without zero ledger")
	}
}

func legacyDaily(db *gorm.DB, ac row, date time.Time, expiry time.Time, value string) (row, error) {
	b, err := grant(db, ac, "daily", "daily:"+date.Format("2006-01-02"), dec(value), date.In(zone), &expiry, 0)
	if err != nil {
		return nil, err
	}
	g := row{"account_id": ac["id"], "policy_version_id": 2, "business_date": date.Format("2006-01-02"), "valid_from": date.In(zone), "expires_at": expiry, "bucket_id": b["id"]}
	return b, db.Table("credit_daily_grants").Create(&g).Error
}

func TestMySQLDailyResetLegacyCutoverIsBoundedIdempotentAndSourceOwned(t *testing.T) {
	f := newAdjustmentMySQLFixture(t)
	_, cycle := resetProfile(t, f, "100.000000")
	// Wallet policy 1 grants zero; historical source 2 must still define reset.
	if err := f.db.Table("credit_accounts").Where("id=1").Updates(row{"credit_policy_version_id": 1, "status": "frozen"}).Error; err != nil {
		t.Fatal(err)
	}
	var first uint64
	if err := f.db.Table("credit_policy_versions").Where("id=1").Updates(row{"timezone": "UTC", "reset_time": "00:00:00"}).Error; err != nil {
		t.Fatal(err)
	}
	resetTransaction(t, f, func(db *gorm.DB, ac row) error {
		for i := 1; i <= settlementChunkSize+3; i++ {
			b, err := legacyDaily(db, ac, cycle.AddDate(0, 0, -i), cycle.AddDate(0, 0, 7).In(zone), "100")
			if err != nil {
				return err
			}
			if i == 1 {
				first = platform.Uint(b["id"])
				if err = failedBillingReservation(db, b, 2, "10", "legacy-held", cycle.Add(-time.Minute)); err != nil {
					return err
				}
			}
		}
		return nil
	})
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- NormalizeDailyBuckets(f.ctx, f.db) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	b := debtRow(t, f.db, "credit_buckets", first)
	if !stamp(b["expires_at"]).Equal(cycle) || !dec(b["remaining_pt"]).Equal(dec("10")) || !dec(b["reserved_pt"]).Equal(dec("10")) || !dec(b["granted_pt"]).Equal(dec("100")) {
		t.Fatalf("legacy cap/held/history mismatch: %#v", b)
	}
	if f.count(t, "credit_buckets", "account_id=1 AND remaining_pt>reserved_pt") != 0 || f.count(t, "credit_ledger", "entry_type='expire'") != settlementChunkSize+3 {
		t.Fatal("cutover missed a page or omitted expiration ledger")
	}
	ledger := f.count(t, "credit_ledger", "1=1")
	revision := platform.Uint(debtRow(t, f.db, "credit_accounts", 1)["revision"])
	if err := f.db.Table("credit_policy_versions").Where("id=2").Update("reset_time", "05:30:00").Error; err != nil {
		t.Fatal(err)
	}
	if err := NormalizeDailyBuckets(f.ctx, f.db); err != nil {
		t.Fatal(err)
	}
	if ledger != f.count(t, "credit_ledger", "1=1") || revision != platform.Uint(debtRow(t, f.db, "credit_accounts", 1)["revision"]) {
		t.Fatal("normalization replay changed financial state")
	}
	resetTransaction(t, f, func(db *gorm.DB, ac row) error {
		other, err := failedBillingCharge(db, 1, "3", now().Add(-time.Second))
		if err != nil {
			return err
		}
		if err = payCharge(db, ac, row{"id": 1}, other, now()); err != nil {
			return err
		}
		if !dec(other["settled_pt"]).IsZero() {
			return fmt.Errorf("foreign session spent old reservation")
		}
		own, err := failedBillingCharge(db, 2, "4", now().Add(-time.Second))
		if err != nil {
			return err
		}
		if err = payCharge(db, ac, row{"id": 2}, own, now()); err != nil {
			return err
		}
		r, err := find(db, "credit_reservations", "reservation_key='legacy-held'")
		if err != nil {
			return err
		}
		return releaseReservation(db, r, now())
	})
	b = debtRow(t, f.db, "credit_buckets", first)
	if !dec(b["remaining_pt"]).IsZero() || !dec(b["reserved_pt"]).IsZero() || f.count(t, "credit_ledger", "entry_type='expire' AND amount_delta_pt=-6") != 1 {
		t.Fatalf("expired release revived funds: %#v", b)
	}
	if f.count(t, "usage_charges", "session_id=1 AND expected_pt=3 AND settled_pt=0 AND status='insufficient'") != 1 {
		t.Fatal("reset forgave foreign debt")
	}
}

func TestMySQLDailyResetRefundUsesNormalizedOriginalExpiry(t *testing.T) {
	f := newAdjustmentMySQLFixture(t)
	_, cycle := resetProfile(t, f, "100.000000")
	var bucketID uint64
	resetTransaction(t, f, func(db *gorm.DB, ac row) error {
		b, err := legacyDaily(db, ac, cycle.AddDate(0, 0, -1), cycle.AddDate(0, 0, 7).In(zone), "100")
		if err != nil {
			return err
		}
		bucketID = platform.Uint(b["id"])
		charge, err := failedBillingCharge(db, 1, "100", cycle.Add(-time.Minute))
		if err != nil {
			return err
		}
		return payCharge(db, ac, row{"id": 1}, charge, cycle.Add(-time.Second))
	})
	if err := NormalizeDailyBuckets(f.ctx, f.db); err != nil {
		t.Fatal(err)
	}
	at := failedBillingTask(t, f)
	for range 2 {
		if err := cancelFailedBillingChunk(f, at); err != nil {
			t.Fatal(err)
		}
	}
	b := debtRow(t, f.db, "credit_buckets", bucketID)
	if !dec(b["remaining_pt"]).IsZero() || !stamp(b["expires_at"]).Equal(cycle) || f.count(t, "credit_ledger", "entry_type='refund' AND amount_delta_pt=100") != 1 || f.count(t, "credit_ledger", "entry_type='expire' AND amount_delta_pt=-100") != 1 {
		t.Fatalf("fully consumed legacy source refund revived: %#v", b)
	}
}

func TestMySQLDailyResetRejectsMissingHistory(t *testing.T) {
	for _, kind := range []string{"orphan_bucket", "nil_bucket", "mismatched_date"} {
		t.Run(kind, func(t *testing.T) {
			f := newAdjustmentMySQLFixture(t)
			_, cycle := resetProfile(t, f, "100.000000")
			resetTransaction(t, f, func(db *gorm.DB, ac row) error {
				expiry := cycle.AddDate(0, 0, 7).In(zone)
				if kind == "nil_bucket" {
					g := row{"account_id": 1, "policy_version_id": 2, "business_date": cycle.Format("2006-01-02"), "valid_from": cycle.In(zone), "expires_at": expiry}
					return db.Table("credit_daily_grants").Create(&g).Error
				}
				if kind == "orphan_bucket" {
					_, err := grant(db, ac, "daily", "orphan", dec("100"), cycle.In(zone), &expiry, 0)
					return err
				}
				b, err := legacyDaily(db, ac, cycle, expiry, "100")
				if err != nil {
					return err
				}
				return db.Table("credit_buckets").Where("id=?", b["id"]).Update("business_key", "daily:1900-01-01").Error
			})
			if err := NormalizeDailyBuckets(f.ctx, f.db); err == nil {
				t.Fatal("invalid historical source silently accepted")
			}
		})
	}
}

func TestMySQLDailyResetNeverExtendsExpiryOrRecapsFinalizedGrants(t *testing.T) {
	f := newAdjustmentMySQLFixture(t)
	p, cycle := resetProfile(t, f, "100.000000")
	var oldID, currentID uint64
	shortExpiry := cycle.AddDate(0, 0, -1).Add(time.Minute).In(zone)
	resetTransaction(t, f, func(db *gorm.DB, ac row) error {
		b, err := legacyDaily(db, ac, cycle.AddDate(0, 0, -1), shortExpiry, "100")
		if err != nil {
			return err
		}
		oldID = platform.Uint(b["id"])
		if err = grantDailyCycle(db, ac, p, cycle, cycle); err != nil {
			return err
		}
		current, err := find(db, "credit_buckets", "account_id=1 AND business_key=?", "daily:"+cycle.Format("2006-01-02"))
		if err != nil {
			return err
		}
		currentID = platform.Uint(current["id"])
		return nil
	})
	if err := NormalizeDailyBuckets(f.ctx, f.db); err != nil {
		t.Fatal(err)
	}
	if !stamp(debtRow(t, f.db, "credit_buckets", oldID)["expires_at"]).Equal(shortExpiry) {
		t.Fatal("cutover extended short TTL")
	}
	before := stamp(debtRow(t, f.db, "credit_buckets", currentID)["expires_at"])
	if err := f.db.Table("credit_policy_versions").Where("id=2").Update("reset_time", "05:30:00").Error; err != nil {
		t.Fatal(err)
	}
	if err := NormalizeDailyBuckets(f.ctx, f.db); err != nil {
		t.Fatal(err)
	}
	if !stamp(debtRow(t, f.db, "credit_buckets", currentID)["expires_at"]).Equal(before) {
		t.Fatal("restart reinterpreted a finalized grant with edited policy")
	}
}
