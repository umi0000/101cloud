package billing

import (
	"database/sql"
	"reflect"
	"testing"
	"time"

	"101cloud/backend/internal/platform"
	"gorm.io/gorm"
)

func reportLiveMySQLFixture(t *testing.T, at time.Time) *adjustmentMySQLFixture {
	t.Helper()
	f := newAdjustmentMySQLFixture(t)
	for _, query := range []string{
		`UPDATE policy_versions SET config=JSON_SET(config,'$.storage_charge_mode','free') WHERE id=1`,
		`UPDATE credit_policy_versions SET storage_charge_mode='free' WHERE id=1`,
		`UPDATE nodes SET health='online' WHERE id=1`,
	} {
		if err := f.db.Exec(query).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := f.db.Table("instances").Where("id=1").Updates(row{"observed_state": "running", "desired_state": "running", "observed_at": at}).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Table("usage_sessions").Where("id=2").Updates(row{"cpu_hourly_pt": "3600.000000", "config_snapshot": `{"vcpus":1,"memory_bytes":"0","disk_bytes":"0"}`, "metering_state": "running", "started_at": at.Add(-time.Hour), "last_settled_at": at, "metered_microseconds": uint64(0), "cumulative_expected_pt": "0.000000"}).Error; err != nil {
		t.Fatal(err)
	}
	return f
}

func reportLoadMySQL(t *testing.T, f *adjustmentMySQLFixture, at time.Time) billingLive {
	t.Helper()
	var result billingLive
	// Real MySQL rejects accidental writes/financial locking helpers in this
	// transaction. Repeat calls must produce the same snapshot without settlement.
	err := f.db.Transaction(func(db *gorm.DB) error {
		var account row
		if err := db.Table("credit_accounts").Where("id=1").Take(&account).Error; err != nil {
			return err
		}
		var err error
		result, err = loadBillingLive(db, account, at)
		if err != nil {
			return err
		}
		again, err := loadBillingLive(db, account, at)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(result, again) {
			t.Fatalf("read-only snapshot changed between reads: %#v / %#v", result, again)
		}
		usage, err := loadBillingUsage(db, account, at, true)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(result.Tails, usage.Tails) || !reflect.DeepEqual(result.Rates, usage.Rates) || !reflect.DeepEqual(result.Current, usage.Current) || result.Partial != usage.Partial || !reflect.DeepEqual(result.Reasons, usage.Reasons) {
			t.Fatalf("overview/instances usage mismatch: %#v / %#v", result, usage)
		}
		trend, err := loadBillingUsage(db, account, at, false)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(result.Tails, trend.Tails) || result.Partial != trend.Partial || !reflect.DeepEqual(result.Reasons, trend.Reasons) {
			t.Fatalf("overview/trend usage mismatch: %#v / %#v", result, trend)
		}
		if usage.Forecast.Status != "" || trend.Forecast.Status != "" {
			t.Fatal("light usage predicted wallet exhaustion")
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func reportGrantMySQL(t *testing.T, f *adjustmentMySQLFixture, key, amount string, valid time.Time, expiry *time.Time, owner uint64, held string) {
	t.Helper()
	if err := f.db.Transaction(func(db *gorm.DB) error {
		account := debtRow(t, db, "credit_accounts", 1)
		bucket, err := grant(db, account, "admin", key, dec(amount), valid, expiry, 1)
		if err != nil {
			return err
		}
		if owner != 0 {
			return failedBillingReservation(db, bucket, owner, held, key+":held", valid)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLReportLiveExcludesExpiredReservedAndFutureFunds(t *testing.T) {
	at := reportLiveClock()
	f := reportLiveMySQLFixture(t, at)
	expired := at.Add(-time.Microsecond)
	reportGrantMySQL(t, f, "report-own-expired", "2", at.Add(-time.Hour), &expired, 2, "2")
	reportGrantMySQL(t, f, "report-other-held", "50", at.Add(-time.Hour), nil, 1, "50")
	reportGrantMySQL(t, f, "report-expired-free", "100", at.Add(-time.Hour), &expired, 0, "")
	reportGrantMySQL(t, f, "report-future-free", "100", at.Add(time.Hour), nil, 0, "")
	reportGrantMySQL(t, f, "report-future-held", "3", at.Add(time.Hour), nil, 2, "3")
	reportGrantMySQL(t, f, "report-current-free", "4", at.Add(-time.Hour), nil, 0, "")
	beforeAccount := debtRow(t, f.db, "credit_accounts", 1)
	beforeSession := debtRow(t, f.db, "usage_sessions", 2)
	ledgerCount := f.count(t, "credit_ledger", "1=1")
	live := reportLoadMySQL(t, f, at)
	if live.Partial || live.Current.Status != "ready" || live.Current.HourlyPT == nil || *live.Current.HourlyPT != "3600.000000" || live.Balance.AvailablePT != "4.000000" || live.Balance.ReservedPT != "55.000000" || live.Balance.OutstandingDebtPT != "0.000000" {
		t.Fatalf("live balance/rate mismatch: %#v", live)
	}
	assertReportForecast(t, live.Forecast, "estimated", "4", at.Add(4*time.Second))
	if !reflect.DeepEqual(beforeAccount, debtRow(t, f.db, "credit_accounts", 1)) || !reflect.DeepEqual(beforeSession, debtRow(t, f.db, "usage_sessions", 2)) || ledgerCount != f.count(t, "credit_ledger", "1=1") || f.count(t, "usage_charges", "1=1") != 0 {
		t.Fatal("reporting mutated financial state")
	}
}

func TestMySQLReportLiveUsesAvailableWithoutRedeductingPendingDebtReservedOrTail(t *testing.T) {
	at := reportLiveClock()
	f := reportLiveMySQLFixture(t, at)
	reportGrantMySQL(t, f, "report-own-payment", "2", at.Add(-time.Hour), nil, 2, "2")
	reportGrantMySQL(t, f, "report-free-payment", "5", at.Add(-time.Hour), nil, 0, "")
	if err := f.db.Table("usage_sessions").Where("id=2").Updates(row{"last_settled_at": at.Add(-time.Second), "metered_microseconds": uint64(2000000), "cumulative_expected_pt": "2.000000"}).Error; err != nil {
		t.Fatal(err)
	}
	for index, status := range []string{"insufficient", "pending"} {
		from := at.Add(time.Duration(index-3) * time.Second)
		charge := row{"session_id": 2, "account_id": 1, "from_at": from, "to_at": from.Add(time.Second), "expected_pt": "1.000000", "settled_pt": "0.000000", "status": status, "calculation_snapshot": `{}`}
		if err := f.db.Table("usage_charges").Create(&charge).Error; err != nil {
			t.Fatal(err)
		}
	}
	live := reportLoadMySQL(t, f, at)
	if live.Partial || len(live.Tails) != 1 || live.Tails[0].BaseMicros != 2000000 || money(live.Tails[0].BaseExpected) != "2.000000" || live.Balance.AvailablePT != "4.000000" || live.Balance.OutstandingDebtPT != "1.000000" || live.Balance.ReservedPT != "2.000000" {
		t.Fatalf("debt/tail mismatch: %#v", live)
	}
	// Public available is free5 - reported debt1. Neither held2, pending1 nor
	// the unclosed tail1 changes the static conversion a second time.
	assertReportForecast(t, live.Forecast, "estimated", "4", at.Add(4*time.Second))
}

func TestMySQLReportLiveExpiryBoundaryAndZeroFunding(t *testing.T) {
	at := reportLiveClock()
	f := reportLiveMySQLFixture(t, at)
	assertReportForecast(t, reportLoadMySQL(t, f, at).Forecast, "exhausted", "0", at)
	expiry := at.Add(2 * time.Second)
	reportGrantMySQL(t, f, "report-expiring-free", "5", at.Add(-time.Hour), &expiry, 0, "")
	assertReportForecast(t, reportLoadMySQL(t, f, at).Forecast, "estimated", "5", at.Add(5*time.Second))
	if err := f.db.Table("instances").Where("id=1").Update("observed_at", expiry).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Table("usage_sessions").Where("id=2").Updates(row{"last_settled_at": expiry, "metered_microseconds": uint64(2000000), "cumulative_expected_pt": "2.000000"}).Error; err != nil {
		t.Fatal(err)
	}
	live := reportLoadMySQL(t, f, expiry)
	if live.Balance.AvailablePT != "0.000000" {
		t.Fatalf("free bucket valid at exact expiry: %#v", live.Balance)
	}
	assertReportForecast(t, live.Forecast, "exhausted", "0", expiry)
}

func TestMySQLReportLiveStaticDurationCrossesRefresh(t *testing.T) {
	at := reportLiveClock()
	for _, tc := range []struct{ amount, seconds string }{{"200.000000", "180000"}, {"199.900000", "179910"}, {"199.999999", "179999"}} {
		t.Run(tc.amount, func(t *testing.T) {
			f := reportLiveMySQLFixture(t, at)
			if err := f.db.Table("usage_sessions").Where("id=2").Update("cpu_hourly_pt", "4.000000").Error; err != nil {
				t.Fatal(err)
			}
			expiry := time.Date(at.Year(), at.Month(), at.Day(), 23, 0, 0, 0, zone)
			reportGrantMySQL(t, f, "report-before-refresh", tc.amount, at.Add(-time.Hour), &expiry, 0, "")
			reportGrantMySQL(t, f, "report-after-refresh", "1000", at.Add(time.Hour), nil, 0, "")
			end := at.Add(time.Duration(dec(tc.seconds).IntPart()) * time.Second)
			live := reportLoadMySQL(t, f, at)
			if live.Balance.AvailablePT != tc.amount {
				t.Fatalf("unexpected available: %#v", live.Balance)
			}
			assertReportForecast(t, live.Forecast, "estimated", tc.seconds, end)
			if !end.After(expiry) {
				t.Fatal("static duration did not cross the refresh")
			}
			for _, boundary := range []time.Time{at.Add(time.Minute), at} {
				if err := f.db.Table("credit_buckets").Where("business_key='report-before-refresh'").Update("expires_at", boundary).Error; err != nil {
					t.Fatal(err)
				}
				live = reportLoadMySQL(t, f, at)
				if boundary.After(at) {
					assertReportForecast(t, live.Forecast, "estimated", tc.seconds, end)
				} else {
					if live.Balance.AvailablePT != "0.000000" {
						t.Fatalf("already expired bucket included: %#v", live.Balance)
					}
					assertReportForecast(t, live.Forecast, "exhausted", "0", at)
				}
			}
		})
	}
}

func TestMySQLReportLiveHistoricalPartialAllowsKnownStaticDuration(t *testing.T) {
	at := reportLiveClock()
	f := reportLiveMySQLFixture(t, at)
	reportGrantMySQL(t, f, "report-known-balance", "5", at.Add(-time.Hour), nil, 0, "")
	recordedInstanceCopy(t, f, 2)
	if err := f.db.Table("instances").Where("id=2").Updates(row{"deleted_at": at, "desired_state": "deleted", "observed_state": "deleted"}).Error; err != nil {
		t.Fatal(err)
	}
	session := debtRow(t, f.db, "usage_sessions", 2)
	session["id"], session["instance_id"], session["last_settled_at"] = uint64(3), uint64(2), at.Add(-time.Second)
	delete(session, "active_instance_id")
	delete(session, "allocation_key")
	if err := f.db.Table("usage_sessions").Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	live := reportLoadMySQL(t, f, at)
	if !live.Partial || live.Current.Status != "ready" || live.Current.HourlyPT == nil || *live.Current.HourlyPT != "3600.000000" || !reflect.DeepEqual(live.Reasons, []string{"deleted_session_unclosed"}) {
		t.Fatalf("historical partial/current mismatch: %#v", live)
	}
	assertReportForecast(t, live.Forecast, "estimated", "5", at.Add(5*time.Second))
}

func TestMySQLReportLiveInactiveAndInconsistentWalletUnavailable(t *testing.T) {
	at := reportLiveClock()
	for _, hourly := range []string{"0.000000", "4.000000"} {
		t.Run(hourly, func(t *testing.T) {
			f := reportLiveMySQLFixture(t, at)
			reportGrantMySQL(t, f, "report-wallet-validity", "5", at.Add(-time.Hour), nil, 0, "")
			if err := f.db.Table("usage_sessions").Where("id=2").Update("cpu_hourly_pt", hourly).Error; err != nil {
				t.Fatal(err)
			}
			if err := f.db.Table("credit_accounts").Where("id=1").Update("status", "frozen").Error; err != nil {
				t.Fatal(err)
			}
			live := reportLoadMySQL(t, f, at)
			assertReportForecast(t, live.Forecast, "unavailable", "", time.Time{})
			if live.Forecast.Reason == nil || *live.Forecast.Reason != "account_not_active" {
				t.Fatalf("inactive account lost reason: %#v", live.Forecast)
			}
			if err := f.db.Table("credit_accounts").Where("id=1").Update("status", "active").Error; err != nil {
				t.Fatal(err)
			}
			// Only this isolated fixture permits corruption, to exercise the
			// reader's defensive wallet validity check without weakening SQL.
			if err := f.db.Exec("ALTER TABLE credit_buckets ALTER CHECK ck_credit_buckets_amounts NOT ENFORCED").Error; err != nil {
				t.Fatal(err)
			}
			if err := f.db.Table("credit_buckets").Where("business_key='report-wallet-validity'").Update("reserved_pt", "6").Error; err != nil {
				t.Fatal(err)
			}
			live = reportLoadMySQL(t, f, at)
			assertReportForecast(t, live.Forecast, "unavailable", "", time.Time{})
			if live.Forecast.Reason == nil || *live.Forecast.Reason != "wallet_inconsistent" {
				t.Fatalf("inconsistent wallet lost reason: %#v", live.Forecast)
			}
		})
	}
}

func TestMySQLReportLiveStoppedHeldUnknownAndFailed(t *testing.T) {
	at := reportLiveClock()
	f := reportLiveMySQLFixture(t, at)
	if err := f.db.Table("usage_sessions").Where("id=2").Update("last_settled_at", at.Add(-2*time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	stopped := at.Add(-time.Second)
	if err := f.db.Table("instances").Where("id=1").Updates(row{"observed_state": "stopped", "observed_at": stopped}).Error; err != nil {
		t.Fatal(err)
	}
	live := reportLoadMySQL(t, f, at)
	if live.Partial || live.Current.HourlyPT == nil || *live.Current.HourlyPT != "0.000000" || len(live.Tails) != 1 || !live.Tails[0].To.Equal(stopped) {
		t.Fatalf("fresh STOP tail/current mismatch: %#v", live)
	}
	assertReportForecast(t, live.Forecast, "not_consuming", "", time.Time{})
	if err := f.db.Table("usage_sessions").Where("id=2").Update("metering_state", "held").Error; err != nil {
		t.Fatal(err)
	}
	live = reportLoadMySQL(t, f, at)
	if live.Partial || len(live.Tails) != 0 {
		t.Fatalf("held session generated a tail: %#v", live)
	}
	if err := f.db.Table("usage_sessions").Where("id=2").Update("metering_state", "running").Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Table("instances").Where("id=1").Updates(row{"observed_state": "unknown", "observed_at": at}).Error; err != nil {
		t.Fatal(err)
	}
	live = reportLoadMySQL(t, f, at)
	if !live.Partial || live.Current.Status != "unavailable" || live.Current.HourlyPT != nil || len(live.Tails) != 0 {
		t.Fatalf("unknown observation became zero/estimated usage: %#v", live)
	}
	assertReportForecast(t, live.Forecast, "unavailable", "", time.Time{})
	if err := f.db.Table("usage_charges").Create(&row{"session_id": 2, "account_id": 1, "from_at": at.Add(-time.Second), "to_at": at, "expected_pt": "20", "settled_pt": "0", "status": "insufficient", "calculation_snapshot": `{}`}).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Table("instances").Where("id=1").Update("creation_failed_at", at).Error; err != nil {
		t.Fatal(err)
	}
	live = reportLoadMySQL(t, f, at)
	if live.Partial || live.Current.HourlyPT == nil || *live.Current.HourlyPT != "0.000000" || live.Balance.OutstandingDebtPT != "0.000000" || len(live.Tails) != 0 {
		t.Fatalf("failed creation survived in current/debt/tail: %#v", live)
	}
}

func TestMySQLReportLiveOfflinePendingAndUnfundedDebt(t *testing.T) {
	at := reportLiveClock()
	f := reportLiveMySQLFixture(t, at)
	reportGrantMySQL(t, f, "report-backlog-free", "5", at.Add(-time.Hour), nil, 0, "")
	charge, err := insert(f.db, "usage_charges", row{"session_id": 1, "account_id": 1, "from_at": at.Add(-2 * time.Second), "to_at": at.Add(-time.Second), "expected_pt": "2", "settled_pt": "0", "status": "pending", "calculation_snapshot": `{"offline_deferred":true}`})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.Table("nodes").Where("id=1").Update("health", "offline").Error; err != nil {
		t.Fatal(err)
	}
	live := reportLoadMySQL(t, f, at)
	if !live.Partial || live.Current.Status != "unavailable" || live.Forecast.Status != "unavailable" || len(live.Tails) != 0 {
		t.Fatalf("offline observation reported a reliable horizon: %#v", live)
	}
	if err := f.db.Table("nodes").Where("id=1").Update("health", "online").Error; err != nil {
		t.Fatal(err)
	}
	live = reportLoadMySQL(t, f, at)
	assertReportForecast(t, live.Forecast, "estimated", "5", at.Add(5*time.Second))
	if err := f.db.Table("usage_charges").Where("id=?", platform.Uint(charge["id"])).Updates(row{"status": "insufficient", "expected_pt": "10"}).Error; err != nil {
		t.Fatal(err)
	}
	live = reportLoadMySQL(t, f, at)
	if live.Balance.AvailablePT != "-5.000000" || live.Balance.OutstandingDebtPT != "10.000000" {
		t.Fatalf("negative/debt balance lost: %#v", live.Balance)
	}
	assertReportForecast(t, live.Forecast, "exhausted", "0", at)
}

func TestMySQLReportLiveTailRoundingBoundary(t *testing.T) {
	at := reportLiveClock()
	f := reportLiveMySQLFixture(t, at)
	if err := f.db.Table("usage_sessions").Where("id=2").Updates(row{"cpu_hourly_pt": "0.000001", "last_settled_at": at.Add(-time.Microsecond), "metered_microseconds": uint64(1799999999), "cumulative_expected_pt": "0.000000"}).Error; err != nil {
		t.Fatal(err)
	}
	reportGrantMySQL(t, f, "report-one-micro-free-and-held", "0.000002", at.Add(-time.Hour), nil, 2, "0.000001")
	live := reportLoadMySQL(t, f, at)
	if live.Partial || len(live.Tails) != 1 {
		t.Fatalf("rounding tail missing: %#v", live)
	}
	tail := live.Tails[0]
	if got := expected(tail.Session, tail.BaseMicros+1).Sub(tail.BaseExpected); money(got) != "0.000001" {
		t.Fatalf("tail rounded independently: %s", got)
	}
	if live.Balance.AvailablePT != "0.000001" || live.Balance.ReservedPT != "0.000001" {
		t.Fatalf("rounding tail changed public available: %#v", live.Balance)
	}
	assertReportForecast(t, live.Forecast, "estimated", "3600", at.Add(time.Hour))
}

func TestMySQLReportLiveGraceWaivedAndStoredExemption(t *testing.T) {
	at := reportLiveClock()
	f := reportLiveMySQLFixture(t, at)
	exhausted := at.Add(-time.Second)
	event := row{"account_id": 1, "user_id": 2, "policy_version_id": 1, "status": "grace", "exhausted_at": exhausted, "shutdown_deadline": at.Add(time.Minute), "policy_snapshot": jsonValue(validCreditPolicy())}
	inserted, err := insert(f.db, "credit_enforcements", event)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.Table("credit_enforcement_instances").Create(&row{"enforcement_id": inserted["id"], "instance_id": 1}).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Table("usage_sessions").Where("id=2").Update("last_settled_at", exhausted).Error; err != nil {
		t.Fatal(err)
	}
	live := reportLoadMySQL(t, f, at)
	if live.Partial || live.Current.HourlyPT == nil || *live.Current.HourlyPT != "0.000000" || len(live.Tails) != 0 {
		t.Fatalf("waived grace was charged: %#v", live)
	}
	assertReportForecast(t, live.Forecast, "not_consuming", "", time.Time{})
	if err := f.db.Table("usage_sessions").Where("id=2").Updates(row{"last_settled_at": at.Add(-2 * time.Second), "billing_mode": "admin_exempt"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Table("credit_enforcements").Where("id=?", inserted["id"]).Update("status", "cancelled").Error; err != nil {
		t.Fatal(err)
	}
	live = reportLoadMySQL(t, f, at)
	if live.Partial || live.Current.Status != "ready" || live.Current.HourlyPT == nil || *live.Current.HourlyPT != "0.000000" || len(live.Tails) != 0 {
		t.Fatalf("stored exemption followed current instance mode: %#v", live)
	}
	assertReportForecast(t, live.Forecast, "not_consuming", "", time.Time{})
}

func TestMySQLReportUsageWithoutWalletTables(t *testing.T) {
	at := reportLiveClock()
	f := reportLiveMySQLFixture(t, at)
	if err := f.db.Table("usage_sessions").Where("id=2").Update("last_settled_at", at.Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	account := debtRow(t, f.db, "credit_accounts", 1)
	want, err := loadBillingLive(f.db, account, at)
	if err != nil {
		t.Fatal(err)
	}
	// The fixture owns an isolated database. Removing the financial tables from
	// name resolution proves independence with real SQL, not query mocks.
	if err := f.db.Exec("RENAME TABLE credit_buckets TO report_hidden_buckets, credit_reservations TO report_hidden_reservations, usage_charges TO report_hidden_charges").Error; err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := f.db.Exec("RENAME TABLE report_hidden_buckets TO credit_buckets, report_hidden_reservations TO credit_reservations, report_hidden_charges TO usage_charges").Error; err != nil {
			t.Error(err)
		}
	}()
	for _, needRates := range []bool{false, true} {
		got, err := loadBillingUsage(f.db, account, at, needRates)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(want.Tails, got.Tails) || want.Partial != got.Partial || !reflect.DeepEqual(want.Reasons, got.Reasons) {
			t.Fatalf("usage changed without financial tables: %#v / %#v", want, got)
		}
		if needRates && (!reflect.DeepEqual(want.Rates, got.Rates) || !reflect.DeepEqual(want.Current, got.Current)) {
			t.Fatalf("rates changed without financial tables: %#v / %#v", want, got)
		}
		if got.Forecast.Status != "" || got.Balance != (billingBalanceDTO{}) {
			t.Fatalf("light usage returned financial prediction: %#v", got)
		}
	}
	if _, err := loadBillingLive(f.db, account, at); err == nil {
		t.Fatal("full overview unexpectedly succeeded without financial tables")
	}
}
