package billing

import (
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestMySQLObservedStopCapturesBeforeClosingAndDoesNotRepeat(t *testing.T) {
	at := now()
	f := newRecordedRateFixture(t, at)
	if err := f.db.Table("instances").Where("id=1").Updates(row{"observed_state": "stopped", "observed_at": at}).Error; err != nil {
		t.Fatal(err)
	}
	for range 2 {
		recordedInstanceTransaction(t, f, 1, func(db *gorm.DB, instance row) error {
			return reconcileInstanceUsage(f.ctx, db, instance, at)
		})
	}
	event := recordedEvent(t, f.db, 2, "observed.stop:1:2")
	assertRecordedRate(t, event, recordedRate("2.000001"), recordedRate("0.000000"))
	if event.EventType != "instance.stop" || !event.Published || f.count(t, "billing_events", "user_id=2") != 1 {
		t.Fatalf("repeated observation manufactured stop events: %+v", event)
	}
}

func TestMySQLObservationWinningWorkerRaceKeepsOriginalTaskKey(t *testing.T) {
	for _, kind := range []string{"instance.create", "instance.start"} {
		t.Run(kind, func(t *testing.T) {
			at := now()
			f := newRecordedRateFixture(t, at)
			if err := f.db.Table("usage_sessions").Where("id=2").Update("metering_state", "held").Error; err != nil {
				t.Fatal(err)
			}
			if err := f.db.Table("instances").Where("id=1").Updates(row{"observed_state": "running", "observed_at": at}).Error; err != nil {
				t.Fatal(err)
			}
			task, err := insert(f.db, "tasks", row{"task_type": kind, "idempotency_scope": "event-observation", "idempotency_key": kind, "is_instance_mutation": 1, "user_id": 2, "node_id": 1, "instance_id": 1, "status": "running", "lease_owner": "event-test", "lease_until": at.Add(time.Minute), "parameters_redacted": "{}", "result_redacted": "{}"})
			if err != nil {
				t.Fatal(err)
			}
			key := "task:" + sid(task["id"]) + ":" + kind
			for range 2 {
				recordedInstanceTransaction(t, f, 1, func(db *gorm.DB, instance row) error {
					return reconcileInstanceUsage(f.ctx, db, instance, at)
				})
			}
			original := recordedEvent(t, f.db, 2, key)
			assertRecordedRate(t, original, recordedRate("0.000000"), recordedRate("2.000001"))
			if original.Published != (kind == "instance.start") {
				t.Fatalf("creation was published before delivery: %+v", original)
			}
			recordedInstanceTransaction(t, f, 1, func(db *gorm.DB, instance row) error {
				return ConfirmInstanceUsage(f.ctx, db, instance, "running", kind, key, at.Add(time.Second))
			})
			confirmed := recordedEvent(t, f.db, 2, key)
			if confirmed.ID != original.ID || !confirmed.OccurredAt.Equal(original.OccurredAt) || f.count(t, "billing_events", "user_id=2") != 1 {
				t.Fatalf("worker duplicated or retimed observation event: original=%+v confirmed=%+v", original, confirmed)
			}
			assertRecordedRate(t, confirmed, recordedRate("0.000000"), recordedRate("2.000001"))
		})
	}
}
