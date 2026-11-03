package billing

import (
	"testing"
	"time"

	"101cloud/backend/internal/platform"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

func debtRetryFixture(t *testing.T) (*gorm.DB, row, time.Time) {
	t.Helper()
	_, root := openBillingMySQL(t, "ci_debt_retry_")
	db := root.Begin()
	if db.Error != nil {
		t.Fatal(db.Error)
	}
	t.Cleanup(func() { db.Rollback() })
	// Connection-local tables retain exact decimal and timestamp semantics without
	// manufacturing unrelated instance/node fixtures. The account stays locked.
	for table, definition := range map[string]string{
		"credit_accounts":     "id BIGINT PRIMARY KEY,user_id BIGINT,version BIGINT DEFAULT 0,revision BIGINT DEFAULT 1",
		"credit_buckets":      "id BIGINT AUTO_INCREMENT PRIMARY KEY,account_id BIGINT,source_type VARCHAR(24),business_key VARBINARY(191),granted_pt DECIMAL(20,6),remaining_pt DECIMAL(20,6),reserved_pt DECIMAL(20,6),valid_from DATETIME(6),expires_at DATETIME(6),version BIGINT DEFAULT 0,revision BIGINT DEFAULT 1,CHECK(remaining_pt>=reserved_pt AND reserved_pt>=0)",
		"usage_sessions":      "id BIGINT PRIMARY KEY,account_id BIGINT,status VARCHAR(24),billing_mode VARCHAR(24)",
		"usage_charges":       "id BIGINT PRIMARY KEY,account_id BIGINT,session_id BIGINT,expected_pt DECIMAL(20,6),settled_pt DECIMAL(20,6),status VARCHAR(24),settled_at DATETIME(6),revision BIGINT DEFAULT 1,updated_at DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),CHECK(expected_pt>=settled_pt AND settled_pt>=0)",
		"credit_reservations": "id BIGINT PRIMARY KEY,account_id BIGINT,session_id BIGINT,bucket_id BIGINT,reserved_pt DECIMAL(20,6),settled_pt DECIMAL(20,6),released_pt DECIMAL(20,6),status VARCHAR(24),closed_at DATETIME(6),revision BIGINT DEFAULT 1,CHECK(reserved_pt>=settled_pt+released_pt)",
		"credit_ledger":       "id BIGINT AUTO_INCREMENT PRIMARY KEY,account_id BIGINT,bucket_id BIGINT,business_key VARBINARY(191),entry_type VARCHAR(24),amount_delta_pt DECIMAL(20,6),reserved_delta_pt DECIMAL(20,6),remaining_after_pt DECIMAL(20,6),reserved_after_pt DECIMAL(20,6),charge_id BIGINT,reservation_id BIGINT,created_at DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6)",
	} {
		if err := db.Exec("CREATE TEMPORARY TABLE " + table + " (" + definition + ") ENGINE=InnoDB").Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, sql := range []string{
		"INSERT INTO credit_accounts(id,user_id) VALUES(1,1)",
		"INSERT INTO usage_sessions VALUES(1,1,'ended','credit'),(2,1,'active','credit')",
		"INSERT INTO usage_charges(id,account_id,session_id,expected_pt,settled_pt,status,revision,updated_at) VALUES(1,1,1,3,0,'insufficient',7,'2026-11-01 00:00:00'),(2,1,2,2,0,'insufficient',8,'2026-11-01 00:00:00')",
	} {
		if err := db.Exec(sql).Error; err != nil {
			t.Fatal(err)
		}
	}
	ac, err := platform.Get(db, "credit_accounts", 1, true)
	if err != nil {
		t.Fatal(err)
	}
	return db, ac, time.Date(2026, 11, 3, 12, 0, 0, 0, zone)
}

func debtRow(t *testing.T, db *gorm.DB, table string, id uint64) row {
	t.Helper()
	r, err := platform.Get(db, table, id, false)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func assertDebtUnchanged(t *testing.T, db *gorm.DB, before row) {
	t.Helper()
	after := debtRow(t, db, "usage_charges", platform.Uint(before["id"]))
	for _, key := range []string{"status", "settled_pt", "expected_pt", "revision"} {
		if platform.Text(before[key]) != platform.Text(after[key]) {
			t.Fatalf("retry changed %s: before=%v after=%v", key, before[key], after[key])
		}
	}
	if !stamp(before["updated_at"]).Equal(stamp(after["updated_at"])) {
		t.Fatal("an unpaid retry changed the visible charge timestamp")
	}
}

func TestMySQLDebtRetryWithoutFundsPreservesChargeAndLedger(t *testing.T) {
	db, ac, at := debtRetryFixture(t)
	before := debtRow(t, db, "usage_charges", 1)
	session := debtRow(t, db, "usage_sessions", 1)
	for i := range 3 {
		if err := payCharge(db, ac, session, debtRow(t, db, "usage_charges", 1), at.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err := restore(db, ac, at.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	assertDebtUnchanged(t, db, before)
	var ledger int64
	if err := db.Table("credit_ledger").Count(&ledger).Error; err != nil || ledger != 0 {
		t.Fatalf("unpaid retry created financial entries: %d %v", ledger, err)
	}
	// An initial attempt still has to establish insufficiency, unlike a retry.
	if err := db.Exec("UPDATE usage_charges SET status='pending' WHERE id=2").Error; err != nil {
		t.Fatal(err)
	}
	if err := payCharge(db, ac, debtRow(t, db, "usage_sessions", 2), debtRow(t, db, "usage_charges", 2), at); err != nil {
		t.Fatal(err)
	}
	charge := debtRow(t, db, "usage_charges", 2)
	if platform.Text(charge["status"]) != "insufficient" || platform.Uint(charge["revision"]) != 9 || charge["settled_at"] != nil {
		t.Fatalf("first failure was not persisted: %+v", charge)
	}
}

func TestMySQLDebtRecoveryPartialPaymentAndTopUpRemainExact(t *testing.T) {
	db, ac, at := debtRetryFixture(t)
	if _, err := grant(db, ac, "admin", "partial", decimal.NewFromInt(1), at, nil, 0); err != nil {
		t.Fatal(err)
	}
	if err := restore(db, ac, at); err != nil {
		t.Fatal(err)
	}
	first := debtRow(t, db, "usage_charges", 1)
	if platform.Text(first["status"]) != "insufficient" || money(dec(first["settled_pt"])) != "1.000000" || platform.Uint(first["revision"]) != 8 {
		t.Fatalf("partial debt payment lost: %+v", first)
	}
	if err := restore(db, ac, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	assertDebtUnchanged(t, db, first)
	if _, err := grant(db, ac, "admin", "topup", decimal.NewFromInt(4), at.Add(2*time.Minute), nil, 0); err != nil {
		t.Fatal(err)
	}
	if err := restore(db, ac, at.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[uint64]string{1: "3.000000", 2: "2.000000"} {
		c := debtRow(t, db, "usage_charges", id)
		if platform.Text(c["status"]) != "settled" || money(dec(c["settled_pt"])) != want || c["settled_at"] == nil {
			t.Fatalf("top-up did not clear historical debt %d: %+v", id, c)
		}
		var paid string
		if err := db.Table("credit_ledger").Select("COALESCE(SUM(-amount_delta_pt),0)").Where("charge_id=? AND entry_type='settle'", id).Scan(&paid).Error; err != nil || money(dec(paid)) != want {
			t.Fatalf("settlement ledger mismatch: %s %v", paid, err)
		}
	}
	before := debtRow(t, db, "usage_charges", 1)
	if err := restore(db, ac, at.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	assertDebtUnchanged(t, db, before)
}

func TestMySQLDebtRecoveryKeepsExpiredReservationOwnership(t *testing.T) {
	db, ac, at := debtRetryFixture(t)
	for _, sql := range []string{
		"INSERT INTO credit_buckets(id,account_id,granted_pt,remaining_pt,reserved_pt,valid_from,expires_at) VALUES(1,1,3,3,3,'2026-11-01','2026-11-02'),(2,1,9,9,0,'2026-11-01','2026-11-02'),(3,1,9,9,0,'2026-11-04',NULL)",
		"INSERT INTO credit_reservations(id,account_id,session_id,bucket_id,reserved_pt,settled_pt,released_pt,status) VALUES(1,1,2,1,3,0,0,'held')",
	} {
		if err := db.Exec(sql).Error; err != nil {
			t.Fatal(err)
		}
	}
	first := debtRow(t, db, "usage_charges", 1)
	if err := restore(db, ac, at); err != nil {
		t.Fatal(err)
	}
	assertDebtUnchanged(t, db, first)
	second := debtRow(t, db, "usage_charges", 2)
	if platform.Text(second["status"]) != "settled" || money(dec(second["settled_pt"])) != "2.000000" {
		t.Fatalf("own expired reservation became unpayable: %+v", second)
	}
	bucket := debtRow(t, db, "credit_buckets", 1)
	if money(dec(bucket["remaining_pt"])) != "1.000000" || money(dec(bucket["reserved_pt"])) != "1.000000" {
		t.Fatalf("reservation conservation failed: %+v", bucket)
	}
	for _, id := range []uint64{2, 3} {
		b := debtRow(t, db, "credit_buckets", id)
		if money(dec(b["remaining_pt"])) != "9.000000" {
			t.Fatal("expired unreserved or future credit was spent")
		}
	}
}
