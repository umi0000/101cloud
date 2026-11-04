package billing

import (
	"testing"
	"time"

	"101cloud/backend/internal/platform"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

func stoppedUsageFixture(t *testing.T) (*adjustmentMySQLFixture, time.Time) {
	t.Helper()
	f := newAdjustmentMySQLFixture(t)
	at := now().Add(-20 * time.Second)
	for _, statement := range []string{
		"UPDATE nodes SET health='online' WHERE id=1",
		"UPDATE instances SET cpu_hourly_pt=3600,memory_gib_hourly_pt=0,disk_gib_hourly_pt=3600 WHERE id=1",
		"INSERT INTO gpu_price_versions(id,code,version,name,gpu_model,access_mode,gpu_hourly_pt,status,effective_at,created_by_user_id) VALUES(1,'stop-gpu','1','Stop GPU','stop-model','shared',3600,'active','2000-01-01',1)",
		"INSERT INTO node_gpus(id,node_id,uuid,model,memory_mib,max_allocations,status,last_seen_at) VALUES(1,1,'GPU-stop','stop-model',8192,8,'available',NOW(6))",
		"INSERT INTO gpu_allocations(id,instance_id,user_id,node_id,gpu_id,gpu_price_version_id,gpu_hourly_pt,status,reserved_at,configured,observed) VALUES(1,1,2,1,1,1,3600,'active',NOW(6),true,false)",
	} {
		if err := f.db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := f.db.Table("gpu_allocations").Where("id=1").Update("reserved_at", at.Add(-time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Table("usage_sessions").Where("status='active'").Updates(row{"status": "ended", "ended_at": at.Add(-time.Hour), "last_settled_at": at.Add(-time.Hour)}).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Table("instances").Where("id=1").Updates(row{"observed_state": "stopped", "observed_at": at}).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Transaction(func(db *gorm.DB) error {
		ac, err := account(db, 2)
		if err != nil {
			return err
		}
		_, err = grant(db, ac, "admin", "stop-funds", decimal.NewFromInt(10000), at.Add(-time.Hour), nil, 1)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return f, at
}

func stoppedUsageEvent(t *testing.T, f *adjustmentMySQLFixture, event string, at time.Time, ids ...uint64) {
	t.Helper()
	if err := f.db.Transaction(func(db *gorm.DB) error {
		instance, err := platform.Get(db, "instances", 1, false)
		if err != nil {
			return err
		}
		if event == "running" || event == "stopped" || event == "resize_stopped" {
			state := event
			if state == "resize_stopped" {
				state = "stopped"
			}
			instance["observed_state"], instance["observed_at"] = state, at
			if err = db.Table("instances").Where("id=1").Updates(row{"observed_state": state, "observed_at": at}).Error; err != nil {
				return err
			}
		}
		return instanceUsageAt(f.ctx, db, instance, event, ids, at)
	}); err != nil {
		t.Fatal(err)
	}
}

func assertStoppedUsageAmount(t *testing.T, f *adjustmentMySQLFixture, expected, settled string) {
	t.Helper()
	var total struct{ Expected, Settled string }
	if err := f.db.Table("usage_charges").Select("COALESCE(SUM(expected_pt),0) expected,COALESCE(SUM(settled_pt),0) settled").Scan(&total).Error; err != nil {
		t.Fatal(err)
	}
	if !dec(total.Expected).Equal(dec(expected)) || !dec(total.Settled).Equal(dec(settled)) {
		t.Fatalf("charges expected=%s settled=%s; want %s/%s", total.Expected, total.Settled, expected, settled)
	}
}

func TestMySQLStoppedAllResourcesResumeKeptGPUWithoutDuplicates(t *testing.T) {
	for _, stopEvent := range []string{"stopped", "resize_stopped"} {
		t.Run(stopEvent, func(t *testing.T) {
			f, at := stoppedUsageFixture(t)
			stoppedUsageEvent(t, f, "running", at)
			stoppedUsageEvent(t, f, "running", at.Add(time.Second))
			if n := f.count(t, "usage_sessions", "status='active'"); n != 3 {
				t.Fatalf("duplicate running created %d sessions", n)
			}
			stoppedUsageEvent(t, f, stopEvent, at.Add(5*time.Second))
			assertStoppedUsageAmount(t, f, "15", "15")
			if n := f.count(t, "usage_sessions", "status='active'"); n != 0 {
				t.Fatalf("STOP retained %d active sessions", n)
			}
			if n := f.count(t, "credit_reservations", "status='held'"); n != 0 {
				t.Fatalf("paid STOP retained %d reservations", n)
			}
			allocation := debtRow(t, f.db, "gpu_allocations", 1)
			if platform.Text(allocation["status"]) != "active" || !platform.Bool(allocation["configured"]) {
				t.Fatal("billing STOP changed the GPU binding")
			}
			stoppedUsageEvent(t, f, "stopped", at.Add(6*time.Second))
			stoppedUsageEvent(t, f, "running", at.Add(10*time.Second))
			stoppedUsageEvent(t, f, "running", at.Add(11*time.Second))
			stoppedUsageEvent(t, f, "stopped", at.Add(15*time.Second))
			assertStoppedUsageAmount(t, f, "30", "30")
			if err := f.db.Table("gpu_allocations").Where("id=1").Updates(row{"status": "released", "configured": false, "release_verified": true, "released_at": at.Add(16 * time.Second)}).Error; err != nil {
				t.Fatal(err)
			}
			stoppedUsageEvent(t, f, "running", at.Add(17*time.Second))
			if n := f.count(t, "usage_sessions", "status='active' AND charge_kind='gpu'"); n != 0 {
				t.Fatal("released GPU was resurrected on START")
			}
			stoppedUsageEvent(t, f, "stopped", at.Add(19*time.Second))
			assertStoppedUsageAmount(t, f, "34", "34")
		})
	}
}

func TestMySQLStoppedAdmissionAndGPUAttachNeverMeterOrRenew(t *testing.T) {
	f, at := stoppedUsageFixture(t)
	stoppedUsageEvent(t, f, "disk_reserved", at)
	stoppedUsageEvent(t, f, "gpu_reserved", at, 1)
	stoppedUsageEvent(t, f, "gpu_attached", at.Add(time.Second), 1)
	reservations := f.count(t, "credit_reservations", "status='held'")
	if n := f.count(t, "usage_sessions", "status='active' AND metering_state='running'"); n != 0 {
		t.Fatal("admission/attachment was treated as guest runtime")
	}
	app := platform.New(f.db, platform.Config{})
	for range 2 {
		if err := settleAll(f.ctx, app); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.count(t, "credit_reservations", "status='held'"); n != reservations {
		t.Fatal("stopped held admission renewed reservations")
	}
	if n := f.count(t, "usage_charges", "1=1"); n != 0 {
		t.Fatal("stopped held admission generated charges")
	}
	stoppedUsageEvent(t, f, "running", now())
	if n := f.count(t, "usage_sessions", "status='active' AND metering_state='running'"); n != 3 {
		t.Fatalf("START did not promote all held resources: %d", n)
	}
}

func TestMySQLObservedStopStartConvergesHistoricalSessionsWithoutRefund(t *testing.T) {
	f, at := stoppedUsageFixture(t)
	stoppedUsageEvent(t, f, "running", at)
	if err := f.db.Transaction(func(db *gorm.DB) error {
		ac, err := account(db, 2)
		if err != nil {
			return err
		}
		instance := debtRow(t, db, "instances", 1)
		p, err := instanceCreditPolicy(db, instance)
		if err != nil {
			return err
		}
		ss, err := rows(db, "usage_sessions", "status='active'")
		if err != nil {
			return err
		}
		for _, session := range ss {
			if err = settleSession(db, ac, p, session, at.Add(10*time.Second)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Table("instances").Where("id=1").Updates(row{"desired_state": "running", "observed_state": "stopped", "observed_at": at.Add(5 * time.Second)}).Error; err != nil {
		t.Fatal(err)
	}
	app := platform.New(f.db, platform.Config{})
	for range 2 {
		if err := settleAll(f.ctx, app); err != nil {
			t.Fatal(err)
		}
	}
	assertStoppedUsageAmount(t, f, "30", "30")
	if n := f.count(t, "usage_sessions", "status='active'"); n != 0 {
		t.Fatal("observed STOP did not converge legacy GPU/storage sessions")
	}
	if err := f.db.Table("instances").Where("id=1").Updates(row{"desired_state": "stopped", "observed_state": "running", "observed_at": at.Add(15 * time.Second)}).Error; err != nil {
		t.Fatal(err)
	}
	if err := settleAll(f.ctx, app); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, "usage_sessions", "status='active' AND started_at>='"+at.Add(15*time.Second).Format("2006-01-02 15:04:05.000000")+"'"); n != 3 {
		t.Fatal("external START failed to resume all resources at observation boundary")
	}
}

func TestMySQLStopPreservesDeferredExpiredReservationOwner(t *testing.T) {
	f, at := stoppedUsageFixture(t)
	if err := f.db.Table("credit_buckets").Where("account_id=1").Update("expires_at", at.Add(2*time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	stoppedUsageEvent(t, f, "running", at)
	if err := f.db.Table("nodes").Where("id=1").Update("health", "offline").Error; err != nil {
		t.Fatal(err)
	}
	stoppedUsageEvent(t, f, "stopped", at.Add(10*time.Second))
	assertStoppedUsageAmount(t, f, "30", "0")
	if n := f.count(t, "credit_reservations", "status='held'"); n != 3 {
		t.Fatalf("deferred STOP lost expired reservation ownership: %d", n)
	}
	if err := f.db.Table("nodes").Where("id=1").Update("health", "online").Error; err != nil {
		t.Fatal(err)
	}
	if err := settleAll(f.ctx, platform.New(f.db, platform.Config{})); err != nil {
		t.Fatal(err)
	}
	assertStoppedUsageAmount(t, f, "30", "30")
	if n := f.count(t, "credit_reservations", "status='held'"); n != 0 {
		t.Fatal("paid stopped deferred reservation was not released")
	}
}

func TestMySQLUnknownStaleAndOfflineAreNotConfirmedStops(t *testing.T) {
	for _, tc := range []struct {
		name, state, health string
		age                 time.Duration
	}{
		{"unknown", "unknown", "online", 0},
		{"stale_stop", "stopped", "online", -2 * time.Minute},
		{"offline_node", "stopped", "offline", 0},
		{"future_stop", "stopped", "online", 2 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, at := stoppedUsageFixture(t)
			stoppedUsageEvent(t, f, "running", at)
			if err := f.db.Table("instances").Where("id=1").Updates(row{"desired_state": "stopped", "observed_state": tc.state, "observed_at": now().Add(tc.age)}).Error; err != nil {
				t.Fatal(err)
			}
			if err := f.db.Table("nodes").Where("id=1").Update("health", tc.health).Error; err != nil {
				t.Fatal(err)
			}
			if err := settleAll(f.ctx, platform.New(f.db, platform.Config{})); err != nil {
				t.Fatal(err)
			}
			if n := f.count(t, "usage_sessions", "status='active' AND metering_state='running'"); n != 3 {
				t.Fatalf("unconfirmed STOP ended %d resources", 3-n)
			}
		})
	}
}

func TestMySQLStopStartPreservesFreeStorageAndAdminExemption(t *testing.T) {
	for _, tc := range []struct {
		name, mode, storage, expected string
		active                        int64
	}{
		{"free_storage", "credit", "free", "20", 2},
		{"admin_exempt", "admin_exempt", "running_only", "0", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, at := stoppedUsageFixture(t)
			if err := f.db.Table("instances").Where("id=1").Update("billing_mode", tc.mode).Error; err != nil {
				t.Fatal(err)
			}
			if err := f.db.Exec("UPDATE policy_versions SET config=JSON_SET(config,'$.storage_charge_mode',?) WHERE id=1", tc.storage).Error; err != nil {
				t.Fatal(err)
			}
			for _, seconds := range []int{0, 10} {
				stoppedUsageEvent(t, f, "running", at.Add(time.Duration(seconds)*time.Second))
				if n := f.count(t, "usage_sessions", "status='active'"); n != tc.active {
					t.Fatalf("running sessions=%d want=%d", n, tc.active)
				}
				stoppedUsageEvent(t, f, "stopped", at.Add(time.Duration(seconds+5)*time.Second))
			}
			assertStoppedUsageAmount(t, f, tc.expected, tc.expected)
			if n := f.count(t, "usage_sessions", "status='active'"); n != 0 {
				t.Fatal("stopped exempt/free instance retained metering sessions")
			}
		})
	}
}

func TestMySQLStoppedDebtRecoveryDoesNotRestartMetering(t *testing.T) {
	f, at := stoppedUsageFixture(t)
	if err := f.db.Table("credit_buckets").Where("account_id=1").Update("remaining_pt", "2.000000").Error; err != nil {
		t.Fatal(err)
	}
	stoppedUsageEvent(t, f, "running", at)
	stoppedUsageEvent(t, f, "stopped", at.Add(5*time.Second))
	assertStoppedUsageAmount(t, f, "15", "2")
	if n := f.count(t, "usage_charges", "status='insufficient'"); n == 0 {
		t.Fatal("STOP discarded pre-stop unpaid usage")
	}
	if err := f.db.Transaction(func(db *gorm.DB) error {
		ac, err := account(db, 2)
		if err != nil {
			return err
		}
		if _, err = grant(db, ac, "admin", "stop-debt-recovery", decimal.NewFromInt(20), now(), nil, 1); err != nil {
			return err
		}
		return restore(db, ac, now())
	}); err != nil {
		t.Fatal(err)
	}
	if err := settleAll(f.ctx, platform.New(f.db, platform.Config{})); err != nil {
		t.Fatal(err)
	}
	assertStoppedUsageAmount(t, f, "15", "15")
	if n := f.count(t, "usage_sessions", "status='active'"); n != 0 {
		t.Fatal("debt recovery reopened stopped sessions")
	}
}

func TestMySQLRestartRunningConfirmationPreservesAllResourceSessions(t *testing.T) {
	f, at := stoppedUsageFixture(t)
	stoppedUsageEvent(t, f, "running", at)
	// Native restart exposes no reliable STOP boundary. Its final RUNNING
	// confirmation must preserve the existing sessions, including bound GPUs.
	stoppedUsageEvent(t, f, "running", at.Add(5*time.Second))
	if n := f.count(t, "usage_sessions", "status='active'"); n != 3 {
		t.Fatalf("restart lost or duplicated resource sessions: %d", n)
	}
	stoppedUsageEvent(t, f, "stopped", at.Add(10*time.Second))
	assertStoppedUsageAmount(t, f, "30", "30")
}
