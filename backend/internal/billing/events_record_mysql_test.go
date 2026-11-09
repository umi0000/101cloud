package billing

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"101cloud/backend/internal/platform"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type recordedBillingEvent struct {
	ID                uint64
	UserID            uint64
	AccountID         sql.NullInt64
	BusinessKey       string
	EventType         string
	Published         bool
	OccurredAt        time.Time
	InstanceID        sql.NullInt64
	InstanceName      sql.NullString
	GrantSource       sql.NullString
	AmountPT          sql.NullString
	AvailableBeforePT sql.NullString
	AvailableAfterPT  sql.NullString
	HourlyBeforePT    sql.NullString
	HourlyAfterPT     sql.NullString
}

func recordedEvent(t *testing.T, db *gorm.DB, user uint64, key string) recordedBillingEvent {
	t.Helper()
	var events []recordedBillingEvent
	if err := db.Table("billing_events").Where("user_id=? AND business_key=?", user, key).Find(&events).Error; err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("event user=%d key=%q: got %d rows, want exactly one", user, key, len(events))
	}
	return events[0]
}

func assertRecordedRate(t *testing.T, event recordedBillingEvent, before, after *string) {
	t.Helper()
	for _, field := range []struct {
		name string
		got  sql.NullString
		want *string
	}{{"before", event.HourlyBeforePT, before}, {"after", event.HourlyAfterPT, after}} {
		if field.want == nil {
			if field.got.Valid {
				t.Fatalf("hourly %s=%q, want SQL NULL: %#v", field.name, field.got.String, event)
			}
		} else if !field.got.Valid || field.got.String != *field.want {
			t.Fatalf("hourly %s=%#v, want %s: %#v", field.name, field.got, *field.want, event)
		}
	}
	if event.AmountPT.Valid || event.AvailableBeforePT.Valid || event.AvailableAfterPT.Valid || event.GrantSource.Valid {
		t.Fatalf("instance event contains grant-only fields: %#v", event)
	}
}

func recordedRate(value string) *string { return &value }

func recordedGrant(t *testing.T, f *adjustmentMySQLFixture, key, amount string, valid time.Time, expiry *time.Time, session uint64, held string) row {
	t.Helper()
	var bucket row
	if err := f.db.Transaction(func(db *gorm.DB) error {
		ac, err := account(db, 2)
		if err != nil {
			return err
		}
		bucket, err = grant(db, ac, "admin", key, dec(amount), valid, expiry, 1)
		if err != nil || session == 0 {
			return err
		}
		return failedBillingReservation(db, bucket, session, held, key+":held", valid)
	}); err != nil {
		t.Fatal(err)
	}
	return bucket
}

func seedRecordedGrantAvailability(t *testing.T, f *adjustmentMySQLFixture) {
	t.Helper()
	at := now()
	expired := at.Add(-time.Hour)
	live := at.Add(time.Hour)
	// Both historical and recent eligible buckets count; reservations belonging
	// to either session remain unavailable. Setup grants themselves record events.
	recordedGrant(t, f, "record-old-live", "10.000001", at.AddDate(-10, 0, 0), &live, 1, "2.000001")
	recordedGrant(t, f, "record-current", "3.000002", at.Add(-time.Minute), nil, 2, "1.000000")
	recordedGrant(t, f, "record-expired", "100.000000", at.Add(-2*time.Hour), &expired, 1, "9.000000")
	recordedGrant(t, f, "record-future", "200.000000", at.Add(time.Hour), nil, 2, "11.000000")
	f.addDebt(t, []string{"5.000003"})
	if err := f.db.Table("usage_charges").Where("id=1").Update("settled_pt", "1.000000").Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Transaction(func(db *gorm.DB) error {
		other, err := insert(db, "credit_accounts", row{"user_id": 1, "credit_policy_version_id": 1, "status": "active"})
		if err != nil {
			return err
		}
		_, err = grant(db, other, "admin", "record-other-wallet", dec("1000"), at.Add(-time.Hour), nil, 1)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLRecordedGrantConsumersUseAllEligibleAccountFunds(t *testing.T) {
	for _, source := range []string{"daily", "redemption", "admin"} {
		t.Run(source, func(t *testing.T) {
			f := newAdjustmentMySQLFixture(t)
			var cycle time.Time
			if source == "daily" {
				_, cycle = resetProfile(t, f, "7.000003")
			}
			seedRecordedGrantAvailability(t, f)
			started := now()
			var bucketID uint64
			var retry func()
			switch source {
			case "daily":
				retry = func() {
					t.Helper()
					if err := dailyGrants(f.ctx, platform.New(f.db, platform.Config{})); err != nil {
						t.Fatal(err)
					}
				}
				retry()
				bucket, err := find(f.db, "credit_buckets", "account_id=1 AND business_key=?", "daily:"+cycle.Format("2006-01-02"))
				if err != nil {
					t.Fatal(err)
				}
				bucketID = platform.Uint(bucket["id"])
			case "redemption":
				secret := "record-event-real-redemption-secret"
				digest := sha256.Sum256([]byte(secret))
				if _, err := insert(f.db, "redemption_codes", row{"code_hash": digest[:], "display_prefix": secret[:8], "grant_pt": "7.000003", "max_uses": 1, "used_count": 0, "valid_from": started.Add(-time.Hour), "expires_at": started.Add(time.Hour), "status": "active", "created_by": 1}); err != nil {
					t.Fatal(err)
				}
				a := platform.New(f.db, platform.Config{})
				router := gin.New()
				router.Use(platform.RequestMiddleware())
				user := router.Group("/api/v1/user", func(c *gin.Context) {
					c.Set("user", platform.User{ID: 2, Role: "user"})
					c.Next()
				}, a.Mutations())
				Register(a, user, router.Group("/api/v1/admin"))
				attempt := 0
				retry = func() {
					t.Helper()
					attempt++
					req := httptest.NewRequest(http.MethodPost, "/api/v1/user/credits/redemptions", strings.NewReader(`{"code":"`+secret+`"}`)).WithContext(f.ctx)
					req.Header.Set("Content-Type", "application/json")
					// Distinct request keys exercise the consumer's business retry,
					// rather than returning the middleware's cached HTTP response.
					req.Header.Set("Idempotency-Key", fmt.Sprintf("record-redeem-%d", attempt))
					response := httptest.NewRecorder()
					router.ServeHTTP(response, req)
					if response.Code != http.StatusCreated {
						t.Fatalf("redemption status=%d: %s", response.Code, response.Body.String())
					}
				}
				retry()
				record, err := find(f.db, "redemption_records", "user_id=2")
				if err != nil {
					t.Fatal(err)
				}
				bucketID = platform.Uint(record["bucket_id"])
			case "admin":
				etag := f.etag(t)
				retry = func() {
					t.Helper()
					result := adjustmentResponse(t, f.call(http.MethodPost, `{"amount_pt":"7.000003"}`, "record-admin-consumer", etag))
					bucketID = platform.Uint(result["bucket_id"])
				}
				retry()
			}
			key := "grant:" + sid(bucketID)
			event := recordedEvent(t, f.db, 2, key)
			if event.EventType != "credit.grant" || !event.Published || event.AccountID != (sql.NullInt64{Int64: 1, Valid: true}) || event.GrantSource != (sql.NullString{String: source, Valid: true}) || event.AmountPT != (sql.NullString{String: "7.000003", Valid: true}) || event.AvailableBeforePT != (sql.NullString{String: "5.999999", Valid: true}) || event.AvailableAfterPT != (sql.NullString{String: "13.000002", Valid: true}) {
				t.Fatalf("grant snapshot excludes eligible history, includes ineligible funds, or loses debt: %#v", event)
			}
			if event.OccurredAt.Before(started) || event.OccurredAt.After(now()) || event.InstanceID.Valid || event.InstanceName.Valid || event.HourlyBeforePT.Valid || event.HourlyAfterPT.Valid {
				t.Fatalf("grant timestamp/nullable fields: %#v", event)
			}
			eventCount := f.count(t, "billing_events", "user_id=2")
			retry()
			if got := recordedEvent(t, f.db, 2, key); !reflect.DeepEqual(got, event) {
				t.Fatalf("consumer retry rewrote original grant: %#v / %#v", event, got)
			}
			if f.count(t, "billing_events", "user_id=2") != eventCount || "grant:"+sid(bucketID) != key {
				t.Fatal("consumer retry created another grant event or bucket")
			}
		})
	}
}

func TestMySQLRecordedZeroDailyGrantAndGrantRollback(t *testing.T) {
	f := newAdjustmentMySQLFixture(t)
	policy, cycle := resetProfile(t, f, "0.000000")
	f.addDebt(t, []string{"2.000001"})
	rollback := errors.New("rollback recorded grant")
	err := f.db.Transaction(func(db *gorm.DB) error {
		ac, err := account(db, 2)
		if err != nil {
			return err
		}
		if _, err = grant(db, ac, "admin", "record-rolled-back-grant", dec("3.000002"), now().Add(-time.Minute), nil, 1); err != nil {
			return err
		}
		if err = grantDailyCycle(db, ac, policy, cycle, cycle); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatalf("grant rollback=%v", err)
	}
	if f.count(t, "billing_events", "user_id=2") != 0 || f.count(t, "credit_buckets", "account_id=1") != 0 || f.count(t, "credit_daily_grants", "account_id=1") != 0 {
		t.Fatal("rolled-back grant left an event, bucket, or consumed daily cycle")
	}
	if err = dailyGrants(f.ctx, platform.New(f.db, platform.Config{})); err != nil {
		t.Fatal(err)
	}
	bucket, err := find(f.db, "credit_buckets", "account_id=1 AND business_key=?", "daily:"+cycle.Format("2006-01-02"))
	if err != nil {
		t.Fatal(err)
	}
	key := "grant:" + sid(bucket["id"])
	event := recordedEvent(t, f.db, 2, key)
	if event.EventType != "credit.grant" || event.GrantSource.String != "daily" || event.AmountPT != (sql.NullString{String: "0.000000", Valid: true}) || event.AvailableBeforePT != (sql.NullString{String: "-2.000001", Valid: true}) || event.AvailableAfterPT != event.AvailableBeforePT || !event.Published {
		t.Fatalf("zero daily grant was not a real exact event: %#v", event)
	}
	for range 2 {
		if err = dailyGrants(f.ctx, platform.New(f.db, platform.Config{})); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(event, recordedEvent(t, f.db, 2, key)) || f.count(t, "billing_events", "user_id=2") != 1 || f.count(t, "credit_daily_grants", "account_id=1") != 1 || f.count(t, "credit_ledger", "account_id=1") != 0 {
		t.Fatal("zero daily retry duplicated/rewrote the event or fabricated a zero ledger entry")
	}
}

func recordedInstanceCopy(t *testing.T, f *adjustmentMySQLFixture, id uint64) row {
	t.Helper()
	instance := debtRow(t, f.db, "instances", 1)
	instance["id"], instance["incus_name"], instance["logical_name"] = id, "record-instance-"+sid(id), "Recorded instance "+sid(id)
	if err := f.db.Table("instances").Create(&instance).Error; err != nil {
		t.Fatal(err)
	}
	return instance
}

func recordedInstanceTransaction(t *testing.T, f *adjustmentMySQLFixture, id uint64, fn func(*gorm.DB, row) error) {
	t.Helper()
	if err := f.db.Transaction(func(db *gorm.DB) error {
		if _, err := account(db, 2); err != nil {
			return err
		}
		instance, err := platform.Get(db, "instances", id, true)
		if err != nil {
			return err
		}
		return fn(db, instance)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLRecordedInstanceFrozenGPUStorageParallelRatesAndRetry(t *testing.T) {
	f, at := stoppedUsageFixture(t)
	if err := f.db.Table("instances").Where("id=1").Updates(row{"vcpus": 2, "memory_bytes": uint64(2147483648), "disk_bytes": uint64(3221225472), "cpu_hourly_pt": "2.000001", "memory_gib_hourly_pt": "0.500001", "disk_gib_hourly_pt": "0.250001"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Table("gpu_allocations").Where("id=1").Update("gpu_hourly_pt", "1.000001").Error; err != nil {
		t.Fatal(err)
	}
	recordedInstanceCopy(t, f, 2)
	if err := f.db.Table("instances").Where("id=2").Updates(row{"vcpus": 1, "cpu_hourly_pt": "3.000001", "memory_gib_hourly_pt": "0.000000", "disk_gib_hourly_pt": "0.000000"}).Error; err != nil {
		t.Fatal(err)
	}
	recordedInstanceTransaction(t, f, 2, func(db *gorm.DB, instance row) error {
		return instanceUsageAt(f.ctx, db, instance, "running", nil, at)
	})
	stoppedUsageEvent(t, f, "disk_reserved", at)
	stoppedUsageEvent(t, f, "gpu_reserved", at, 1)
	// Neither the live catalog nor later instance sizing may replace the
	// hardware and prices frozen into the three held target sessions.
	for _, statement := range []string{
		"UPDATE pricing_versions SET cpu_hourly_pt=999,memory_gib_hourly_pt=999,disk_gib_hourly_pt=999 WHERE id=1",
		"UPDATE gpu_price_versions SET gpu_hourly_pt=999 WHERE id=1",
		"UPDATE gpu_allocations SET gpu_hourly_pt=999 WHERE id=1",
		"UPDATE instances SET vcpus=1,memory_bytes=1073741824,disk_bytes=1073741824,cpu_hourly_pt=999,memory_gib_hourly_pt=999,disk_gib_hourly_pt=999 WHERE id=1",
	} {
		if err := f.db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	key := "record:start:frozen"
	startedAt := at.Add(time.Second)
	recordedInstanceTransaction(t, f, 1, func(db *gorm.DB, instance row) error {
		finish, err := PrepareInstanceEvent(db, instance, "instance.start", key, startedAt, false)
		if err != nil {
			return err
		}
		if err = instanceUsageAt(f.ctx, db, instance, "running", nil, startedAt); err != nil {
			return err
		}
		return finish()
	})
	draft := recordedEvent(t, f.db, 2, key)
	assertRecordedRate(t, draft, recordedRate("3.000001"), recordedRate("9.750009"))
	if draft.Published || draft.InstanceID != (sql.NullInt64{Int64: 1, Valid: true}) || draft.InstanceName != (sql.NullString{String: "Adjustment instance", Valid: true}) || !draft.OccurredAt.Equal(startedAt) {
		t.Fatalf("draft identity/time/publication: %#v", draft)
	}
	recordedInstanceTransaction(t, f, 1, func(db *gorm.DB, instance row) error {
		return ConfirmInstanceUsage(f.ctx, db, instance, "running", "instance.start", key, at.Add(time.Second))
	})
	published := recordedEvent(t, f.db, 2, key)
	draft.Published = true
	if !reflect.DeepEqual(draft, published) {
		t.Fatalf("confirmation must only publish the original draft: %#v / %#v", draft, published)
	}
	stopKey := "record:stop:frozen"
	rollback := errors.New("rollback confirmed usage")
	err := f.db.Transaction(func(db *gorm.DB) error {
		instance, err := platform.Get(db, "instances", 1, true)
		if err != nil {
			return err
		}
		if _, err = account(db, 2); err != nil {
			return err
		}
		if err = ConfirmInstanceUsage(f.ctx, db, instance, "stopped", "instance.stop", stopKey, at.Add(5*time.Second)); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) || f.count(t, "billing_events", "business_key='record:stop:frozen'") != 0 || f.count(t, "usage_sessions", "instance_id=1 AND status='active'") != 3 {
		t.Fatalf("confirmation rollback retained event or ended metering: %v", err)
	}
	recordedInstanceTransaction(t, f, 1, func(db *gorm.DB, instance row) error {
		return ConfirmInstanceUsage(f.ctx, db, instance, "stopped", "instance.stop", stopKey, at.Add(5*time.Second))
	})
	stopped := recordedEvent(t, f.db, 2, stopKey)
	assertRecordedRate(t, stopped, recordedRate("9.750009"), recordedRate("3.000001"))
	if !stopped.Published || !stopped.OccurredAt.Equal(at.Add(5*time.Second)) {
		t.Fatalf("confirmed STOP timestamp/publication: %#v", stopped)
	}
	if err = f.db.Table("usage_sessions").Where("instance_id=2 AND charge_kind='base_compute' AND status='active'").Update("cpu_hourly_pt", "8.000005").Error; err != nil {
		t.Fatal(err)
	}
	if err = f.db.Table("instances").Where("id=1").Update("logical_name", "Renamed after confirmation").Error; err != nil {
		t.Fatal(err)
	}
	recordedInstanceTransaction(t, f, 1, func(db *gorm.DB, instance row) error {
		return ConfirmInstanceUsage(f.ctx, db, instance, "stopped", "instance.stop", stopKey, at.Add(10*time.Second))
	})
	if !reflect.DeepEqual(stopped, recordedEvent(t, f.db, 2, stopKey)) {
		t.Fatal("STOP retry replaced the original rate, timestamp, or instance name")
	}
}

func newRecordedRateFixture(t *testing.T, at time.Time) *adjustmentMySQLFixture {
	t.Helper()
	f := newAdjustmentMySQLFixture(t)
	if err := f.db.Table("nodes").Where("id=1").Update("health", "online").Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Table("usage_sessions").Where("id=2").Updates(row{"config_snapshot": `{"vcpus":1,"memory_bytes":"0","disk_bytes":"0"}`, "cpu_hourly_pt": "2.000001", "metering_state": "running", "last_settled_at": at}).Error; err != nil {
		t.Fatal(err)
	}
	return f
}

func TestMySQLRecordedInstanceUnknownRatesAreNullAndNonConsumingAreZero(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
		want  *string
	}{
		{"running", "", recordedRate("2.000001")},
		{"missing_frozen_quantity", `UPDATE usage_sessions SET config_snapshot=JSON_OBJECT() WHERE id=2`, nil},
		{"non_object_frozen_config", `UPDATE usage_sessions SET config_snapshot=JSON_QUOTE('invalid frozen config') WHERE id=2`, nil},
		{"unknown_metering_state", `UPDATE usage_sessions SET metering_state='unknown' WHERE id=2`, nil},
		{"missing_storage_policy", `UPDATE policy_versions SET config=JSON_REMOVE(config,'$.storage_charge_mode') WHERE id=1`, nil},
		{"unknown_offline_policy", `UPDATE policy_versions SET config=JSON_SET(config,'$.offline_charge_mode','unknown') WHERE id=1`, nil},
		{"deferred_offline_node", `UPDATE nodes SET health='offline' WHERE id=1`, nil},
		{"held_even_with_bad_frozen_config", `UPDATE usage_sessions SET metering_state='held',config_snapshot=JSON_OBJECT() WHERE id=2`, recordedRate("0.000000")},
		{"exempt_even_with_bad_frozen_config", `UPDATE usage_sessions SET billing_mode='admin_exempt',config_snapshot=JSON_OBJECT() WHERE id=2`, recordedRate("0.000000")},
		{"failed_creation", `UPDATE instances SET creation_failed_at=NOW(6) WHERE id=1`, recordedRate("0.000000")},
		{"deleted_instance", `UPDATE instances SET desired_state='deleted',observed_state='deleted',deleted_at=NOW(6) WHERE id=1`, recordedRate("0.000000")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at := now()
			f := newRecordedRateFixture(t, at)
			if tc.query != "" {
				if err := f.db.Exec(tc.query).Error; err != nil {
					t.Fatal(err)
				}
			}
			key := "record:rate:" + tc.name
			recordedInstanceTransaction(t, f, 1, func(db *gorm.DB, instance row) error {
				finish, err := PrepareInstanceEvent(db, instance, "instance.start", key, at, true)
				if err != nil {
					return err
				}
				return finish()
			})
			assertRecordedRate(t, recordedEvent(t, f.db, 2, key), tc.want, tc.want)
		})
	}
}

func TestMySQLRecordedInstanceGraceBoundaryUsesFrozenChargeMode(t *testing.T) {
	for _, mode := range []string{"waived", "accrue"} {
		for _, delta := range []time.Duration{-time.Microsecond, 0, time.Microsecond} {
			t.Run(fmt.Sprintf("%s/%s", mode, delta), func(t *testing.T) {
				exhausted := now()
				f := newRecordedRateFixture(t, exhausted)
				policy := validCreditPolicy()
				policy["grace_charge_mode"] = mode
				event, err := insert(f.db, "credit_enforcements", row{"account_id": 1, "user_id": 2, "policy_version_id": 1, "status": "grace", "exhausted_at": exhausted, "shutdown_deadline": exhausted.Add(10 * time.Minute), "policy_snapshot": jsonValue(policy)})
				if err != nil {
					t.Fatal(err)
				}
				if _, err = insert(f.db, "credit_enforcement_instances", row{"enforcement_id": event["id"], "instance_id": 1, "status": "pending"}); err != nil {
					t.Fatal(err)
				}
				// The active catalog deliberately disagrees with the frozen event.
				other := "waived"
				if mode == "waived" {
					other = "accrue"
				}
				if err = f.db.Exec("UPDATE policy_versions SET config=JSON_SET(config,'$.grace_charge_mode',?) WHERE id=1", other).Error; err != nil {
					t.Fatal(err)
				}
				at := exhausted.Add(delta)
				key := "record:grace:" + mode + ":" + delta.String()
				recordedInstanceTransaction(t, f, 1, func(db *gorm.DB, instance row) error {
					finish, err := PrepareInstanceEvent(db, instance, "instance.stop", key, at, true)
					if err != nil {
						return err
					}
					return finish()
				})
				want := "2.000001"
				if mode == "waived" && delta >= 0 {
					want = "0.000000"
				}
				assertRecordedRate(t, recordedEvent(t, f.db, 2, key), &want, &want)
			})
		}
	}
}

func TestMySQLRecordedInstanceUsesCurrentSessionsAfterWaitingAccountLock(t *testing.T) {
	at := now()
	f := newRecordedRateFixture(t, at)
	recordedInstanceCopy(t, f, 2)
	parallel := row{"account_id": 1, "user_id": 2, "instance_id": 2, "node_id": 1, "charge_kind": "base_compute", "pricing_version_id": 1, "billing_mode": "credit", "config_snapshot": `{"vcpus":1}`, "cpu_hourly_pt": "3.000001", "memory_gib_hourly_pt": "0.000000", "disk_gib_hourly_pt": "0.000000", "gpu_hourly_pt": "0.000000", "status": "active", "metering_state": "running", "started_at": at, "last_settled_at": at}
	parallel["id"] = uint64(3)
	if err := f.db.Table("usage_sessions").Create(&parallel).Error; err != nil {
		t.Fatal(err)
	}
	reader := f.db.WithContext(f.ctx).Begin(&sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if reader.Error != nil {
		t.Fatal(reader.Error)
	}
	defer reader.Rollback()
	instance, err := platform.Get(reader, "instances", 1, false)
	if err != nil {
		t.Fatal(err)
	}
	var connection uint64
	if err = reader.Raw("SELECT CONNECTION_ID()").Scan(&connection).Error; err != nil {
		t.Fatal(err)
	}
	var snapshot string
	const sumSQL = "SELECT CAST(SUM(cpu_hourly_pt) AS DECIMAL(20,6)) FROM usage_sessions WHERE status='active'"
	if err = reader.Raw(sumSQL).Scan(&snapshot).Error; err != nil || snapshot != "5.000002" {
		t.Fatalf("establish old repeatable-read snapshot=%s: %v", snapshot, err)
	}
	writer := f.db.WithContext(f.ctx).Begin()
	if writer.Error != nil {
		t.Fatal(writer.Error)
	}
	defer writer.Rollback()
	if _, err = account(writer, 2); err != nil {
		t.Fatal(err)
	}
	if _, err = platform.Get(writer, "instances", 2, true); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		ac, err := account(reader, 2)
		if err != nil {
			result <- err
			return
		}
		if _, err = platform.Get(reader, "instances", 1, true); err != nil {
			result <- err
			return
		}
		var old string
		if err := reader.Raw(sumSQL).Scan(&old).Error; err != nil {
			result <- err
			return
		}
		if old != "5.000002" {
			result <- fmt.Errorf("consistent snapshot changed after account lock: %s", old)
			return
		}
		available, err := eventAvailable(reader, 1, at)
		if err != nil || available != "7.000000" {
			result <- fmt.Errorf("current-read availability=%s, want 7.000000: %w", available, err)
			return
		}
		if _, err = grant(reader, ac, "admin", "record:current-funds-read", dec("1.000001"), at.Add(-time.Hour), nil, 1); err != nil {
			result <- err
			return
		}
		finish, err := PrepareInstanceEvent(reader, instance, "instance.start", "record:current-session-read", at, true)
		if err != nil {
			result <- err
			return
		}
		if err = reader.Table("usage_sessions").Where("id=2").Update("cpu_hourly_pt", "4.000005").Error; err != nil {
			result <- err
			return
		}
		if err = finish(); err == nil {
			err = reader.Commit().Error
		}
		result <- err
	}()
	// Observe a real InnoDB lock wait rather than hoping a sleeping goroutine
	// reached its SELECT FOR UPDATE before the writer commits.
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	waiting := false
	for !waiting {
		var count int64
		if err = f.db.Raw("SELECT COUNT(*) FROM information_schema.innodb_trx WHERE trx_mysql_thread_id=? AND trx_state='LOCK WAIT'", connection).Scan(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count == 1 {
			waiting = true
			continue
		}
		select {
		case err = <-result:
			t.Fatalf("reader completed before releasing the account lock: %v", err)
		case <-deadline.C:
			t.Fatal("reader did not enter the expected account lock wait")
		case <-ticker.C:
		}
	}
	if err = writer.Table("usage_sessions").Where("id=?", parallel["id"]).Update("cpu_hourly_pt", "7.000003").Error; err != nil {
		t.Fatal(err)
	}
	owner, err := account(writer, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = grant(writer, owner, "admin", "record:writer-funds", dec("10.000001"), at.Add(-time.Hour), nil, 1); err != nil {
		t.Fatal(err)
	}
	if _, err = insert(writer, "usage_charges", row{"account_id": 1, "session_id": parallel["id"], "from_at": at.Add(-time.Second), "to_at": at, "expected_pt": "3.000001", "settled_pt": "0.000000", "status": "insufficient", "calculation_snapshot": "{}"}); err != nil {
		t.Fatal(err)
	}
	if err = writer.Commit().Error; err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-f.ctx.Done():
		t.Fatal(f.ctx.Err())
	}
	event := recordedEvent(t, f.db, 2, "record:current-session-read")
	assertRecordedRate(t, event, recordedRate("9.000004"), recordedRate("11.000008"))
	bucket, err := find(f.db, "credit_buckets", "business_key=?", "record:current-funds-read")
	if err != nil {
		t.Fatal(err)
	}
	granted := recordedEvent(t, f.db, 2, "grant:"+sid(bucket["id"]))
	if granted.AvailableBeforePT.String != "7.000000" || granted.AvailableAfterPT.String != "8.000001" {
		t.Fatalf("grant used stale aggregate funds: %+v", granted)
	}
}

func TestMySQLRecordedInstanceRetainsIndependentlyNullableRateSnapshots(t *testing.T) {
	at := now()
	f := newRecordedRateFixture(t, at)
	if err := f.db.Table("usage_sessions").Where("id=2").Update("config_snapshot", `{}`).Error; err != nil {
		t.Fatal(err)
	}
	recordedInstanceTransaction(t, f, 1, func(db *gorm.DB, instance row) error {
		finish, err := PrepareInstanceEvent(db, instance, "instance.start", "record:null-before", at, true)
		if err != nil {
			return err
		}
		if err = db.Table("usage_sessions").Where("id=2").Update("config_snapshot", `{"vcpus":1}`).Error; err != nil {
			return err
		}
		return finish()
	})
	assertRecordedRate(t, recordedEvent(t, f.db, 2, "record:null-before"), nil, recordedRate("2.000001"))
	recordedInstanceTransaction(t, f, 1, func(db *gorm.DB, instance row) error {
		finish, err := PrepareInstanceEvent(db, instance, "instance.start", "record:null-after", at, true)
		if err != nil {
			return err
		}
		if err = db.Table("usage_sessions").Where("id=2").Update("config_snapshot", `{}`).Error; err != nil {
			return err
		}
		return finish()
	})
	assertRecordedRate(t, recordedEvent(t, f.db, 2, "record:null-after"), recordedRate("2.000001"), nil)
}
