package instances

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"101cloud/backend/internal/platform"
	"gorm.io/gorm"
)

type workerRecordedEvent struct {
	ID             uint64
	EventType      string
	Published      bool
	OccurredAt     time.Time
	InstanceName   sql.NullString
	AccountID      sql.NullInt64
	HourlyBeforePT sql.NullString
	HourlyAfterPT  sql.NullString
}

func workerRecordedEvents(t *testing.T, db *gorm.DB) []workerRecordedEvent {
	t.Helper()
	var result []workerRecordedEvent
	if err := db.Table("billing_events").Order("id").Find(&result).Error; err != nil {
		t.Fatal(err)
	}
	return result
}

func assertWorkerEvent(t *testing.T, event workerRecordedEvent, kind, before, after string, published bool) {
	t.Helper()
	if event.EventType != kind || event.Published != published || !event.HourlyBeforePT.Valid || event.HourlyBeforePT.String != before || !event.HourlyAfterPT.Valid || event.HourlyAfterPT.String != after || event.InstanceName.String != "event-instance" {
		t.Fatalf("event=%+v, want %s %s->%s published=%v", event, kind, before, after, published)
	}
}

func paidWorkerEventFixture(t *testing.T, db *gorm.DB, state string) platform.Row {
	t.Helper()
	for _, mutation := range []struct {
		table, where string
		values       platform.Row
	}{
		{"instances", "id=15", platform.Row{"billing_mode": "credit", "logical_name": "event-instance", "observed_state": state}},
		{"usage_sessions", "instance_id=15", platform.Row{"metering_state": "held"}},
		{"usage_sessions", "id=1", platform.Row{"cpu_hourly_pt": "2.000000"}},
		{"usage_sessions", "id=2", platform.Row{"disk_gib_hourly_pt": "0.050000"}},
	} {
		if err := db.Table(mutation.table).Where(mutation.where).Updates(mutation.values).Error; err != nil {
			t.Fatal(err)
		}
	}
	instance, err := platform.Get(db, "instances", 15, false)
	if err != nil {
		t.Fatal(err)
	}
	return instance
}

func TestMySQLWorkerCreateEventPublishesOnlyDeliveredOriginalSnapshot(t *testing.T) {
	db, a, job, _, _ := creationCleanupDB(t)
	instance := paidWorkerEventFixture(t, db, "pending")
	if len(workerRecordedEvents(t, db)) != 0 {
		t.Fatal("admission created an event")
	}
	// This is the worker's confirmed native RUNNING transaction, before final
	// initialization/delivery. Held admission contributes no hourly rate.
	start := job
	start.Type = "instance.start"
	if err := workerConfirm(t.Context(), a, start, instance, "running", "running", nil); err != nil {
		t.Fatal(err)
	}
	events := workerRecordedEvents(t, db)
	if len(events) != 1 {
		t.Fatalf("draft events=%+v", events)
	}
	assertWorkerEvent(t, events[0], "instance.create", "0.000000", "3.000000", false)
	original := events[0]
	if err := workerConfirm(t.Context(), a, start, instance, "running", "", platform.Row{"provisioning_state": "ready"}); err != nil {
		t.Fatal(err)
	}
	if workerRecordedEvents(t, db)[0].Published {
		t.Fatal("intermediate readiness published creation")
	}
	if err := workerConfirmStep(t.Context(), a, job, instance, "running", "", platform.Row{"provisioning_state": "ready"}, "create.verify"); err != nil {
		t.Fatal(err)
	}
	// A reconciliation wrapper and repeated RUNNING must preserve the first
	// native success snapshot, never introduce an independent create START.
	wrapper := job
	wrapper.ID = 44
	if err := db.Table("tasks").Create(&platform.Row{"id": 44, "task_type": "instance.reconcile", "user_id": 2, "node_id": 1, "instance_id": 15, "status": "running", "lease_owner": "test", "lease_generation": 1, "lease_until": a.Time().Add(time.Minute)}).Error; err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(t.Context(), workerRecoveryKey{}, []uint64{43})
	if err := workerConfirm(ctx, a, wrapper, instance, "running", "running", nil); err != nil {
		t.Fatal(err)
	}
	events = workerRecordedEvents(t, db)
	if len(events) != 1 || events[0].ID != original.ID || !events[0].OccurredAt.Equal(original.OccurredAt) {
		t.Fatalf("recovery rewrote creation: %+v", events)
	}
	assertWorkerEvent(t, events[0], "instance.create", "0.000000", "3.000000", true)
}

func TestMySQLWorkerCreateEventRollbackAndFailedDeliveryRemainHidden(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "rollback", true: "failed_delivery"}[fail], func(t *testing.T) {
			db, a, job, _, _ := creationCleanupDB(t)
			instance := paidWorkerEventFixture(t, db, "pending")
			if !fail {
				cancel := errors.New("rollback confirmation")
				err := db.Transaction(func(tx *gorm.DB) error {
					transactionApp := platform.New(tx, a.Config)
					if err := workerConfirm(t.Context(), transactionApp, job, instance, "running", "running", nil); err != nil {
						return err
					}
					return cancel
				})
				if !errors.Is(err, cancel) {
					t.Fatal(err)
				}
				if len(workerRecordedEvents(t, db)) != 0 {
					t.Fatal("rolled back transaction retained event")
				}
				s, err := platform.Get(db, "usage_sessions", 1, false)
				if err != nil || platform.Text(s["metering_state"]) != "held" {
					t.Fatalf("rollback session=%+v err=%v", s, err)
				}
				return
			}
			if err := workerConfirm(t.Context(), a, job, instance, "running", "running", nil); err != nil {
				t.Fatal(err)
			}
			if err := db.Table("instances").Where("id=15").Update("creation_failed_at", a.Time()).Error; err != nil {
				t.Fatal(err)
			}
			if err := workerConfirmStep(t.Context(), a, job, instance, "stopped", "stopped", platform.Row{"provisioning_state": "ready"}, "create.verify"); err != nil {
				t.Fatal(err)
			}
			events := workerRecordedEvents(t, db)
			if len(events) != 1 || events[0].Published {
				t.Fatalf("failed creation published: %+v", events)
			}
		})
	}
}

func TestMySQLWorkerExemptWithoutWalletRecordsCreateStartAndStop(t *testing.T) {
	db, a, job, instance, _ := creationCleanupDB(t)
	if err := db.Table("instances").Where("id=15").Update("logical_name", "event-instance").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("DELETE FROM credit_accounts WHERE id=7").Error; err != nil {
		t.Fatal(err)
	}
	if err := workerConfirm(t.Context(), a, job, instance, "running", "running", nil); err != nil {
		t.Fatal(err)
	}
	if err := workerConfirmStep(t.Context(), a, job, instance, "running", "", platform.Row{"provisioning_state": "ready"}, "create.verify"); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"instance.start", "instance.stop"} {
		job.Type = kind
		if err := db.Table("tasks").Where("id=43").Update("task_type", kind).Error; err != nil {
			t.Fatal(err)
		}
		state, event := "running", "running"
		if kind == "instance.stop" {
			state, event = "stopped", "stopped"
		}
		if err := workerConfirm(t.Context(), a, job, instance, state, event, platform.Row{"provisioning_state": "ready"}); err != nil {
			t.Fatal(err)
		}
	}
	events := workerRecordedEvents(t, db)
	if len(events) != 3 {
		t.Fatalf("exempt events=%+v", events)
	}
	for i, kind := range []string{"instance.create", "instance.start", "instance.stop"} {
		assertWorkerEvent(t, events[i], kind, "0.000000", "0.000000", true)
		if events[i].AccountID.Valid {
			t.Fatalf("manufactured account: %+v", events[i])
		}
	}
}

func TestMySQLWorkerDeleteEventsFollowConfirmedStopAndRetainName(t *testing.T) {
	for _, force := range []bool{false, true} {
		for _, fail := range []bool{false, true} {
			t.Run(map[bool]string{false: "normal", true: "force"}[force]+map[bool]string{false: "/success", true: "/failed_stop"}[fail], func(t *testing.T) {
				native := &runningLifecycleNative{state: "Running", force: force, failStop: fail}
				db, a, job := deletionDB(t, native, "", "")
				paidWorkerEventFixture(t, db, "running")
				if err := db.Table("usage_sessions").Where("instance_id=15").Update("metering_state", "running").Error; err != nil {
					t.Fatal(err)
				}
				job.Parameters = json.RawMessage(jsonText(platform.Row{"force": force, "delete_data": true}))
				if err := db.Table("tasks").Where("id=43").Update("parameters_redacted", string(job.Parameters)).Error; err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				err := runDelete(ctx, a, job)
				events := workerRecordedEvents(t, db)
				if fail {
					if err == nil || len(events) != 0 {
						t.Fatalf("failed stop err=%v events=%+v", err, events)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if len(events) != 2 {
					t.Fatalf("delete events=%+v", events)
				}
				assertWorkerEvent(t, events[0], "instance.stop", "3.000000", "0.000000", true)
				assertWorkerEvent(t, events[1], "instance.delete", "0.000000", "0.000000", true)
				if err := runDelete(ctx, a, job); err != nil {
					t.Fatal(err)
				}
				if len(workerRecordedEvents(t, db)) != 2 {
					t.Fatal("retry duplicated deletion events")
				}
			})
		}
	}
}

func TestMySQLWorkerCreationDeliveredStoppedKeepsRealRunningAndStopBoundaries(t *testing.T) {
	for _, ran := range []bool{false, true} {
		t.Run(map[bool]string{false: "created_stopped", true: "initialized_then_stopped"}[ran], func(t *testing.T) {
			db, a, job, _, _ := creationCleanupDB(t)
			instance := paidWorkerEventFixture(t, db, "pending")
			if ran {
				if err := workerConfirm(t.Context(), a, job, instance, "running", "running", nil); err != nil {
					t.Fatal(err)
				}
			}
			if err := workerConfirm(t.Context(), a, job, instance, "stopped", "stopped", platform.Row{"provisioning_state": "ready"}); err != nil {
				t.Fatal(err)
			}
			if err := workerConfirm(t.Context(), a, job, instance, "stopped", "stopped", platform.Row{"provisioning_state": "ready"}); err != nil {
				t.Fatal(err)
			}
			events := workerRecordedEvents(t, db)
			if ran {
				if len(events) != 2 {
					t.Fatalf("running creation events=%+v", events)
				}
				assertWorkerEvent(t, events[0], "instance.create", "0.000000", "3.000000", true)
				assertWorkerEvent(t, events[1], "instance.stop", "3.000000", "0.000000", true)
			} else {
				if len(events) != 1 {
					t.Fatalf("stopped creation/retry events=%+v", events)
				}
				assertWorkerEvent(t, events[0], "instance.create", "0.000000", "0.000000", true)
			}
		})
	}
}
