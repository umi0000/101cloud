package monitoring

import (
	"context"
	"testing"
	"time"

	"101cloud/backend/internal/platform"
	"gorm.io/gorm"
)

func TestMySQLConsoleForceStopFencesLatePreparationWrites(t *testing.T) {
	for _, stage := range []string{"dispatch", "operation", "ready"} {
		t.Run(stage, func(t *testing.T) {
			s, row := consoleTestService(t)
			job := platform.Job{ID: 10, Type: "monitoring.console.prepare", LeaseGeneration: 7}
			if err := s.a.DB.Table("tasks").Create(platform.Row{"id": job.ID, "task_type": job.Type, "status": "running", "lease_generation": job.LeaseGeneration, "lease_until": s.a.Time().Add(time.Minute)}).Error; err != nil {
				t.Fatal(err)
			}
			if err := s.a.DB.Table("console_sessions").Where("id=?", row.ID).Updates(platform.Row{"status": "preparing", "prepare_task_id": job.ID, "operation_path": "/1.0/operations/retained", "secrets_ciphertext": []byte("private")}).Error; err != nil {
				t.Fatal(err)
			}
			// Model an I/O completion already in flight when cancellation commits.
			completion := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				<-completion
				changes := platform.Row{"revision": gorm.Expr("revision+1")}
				switch stage {
				case "dispatch":
					changes["prepare_started_at"] = s.a.Time()
				case "operation":
					changes["operation_path"], changes["secrets_ciphertext"] = "/1.0/operations/late", []byte("late-private")
				case "ready":
					changes["status"] = "ready"
				}
				done <- s.prepareUpdate(context.Background(), job, row.ID, changes, stage == "dispatch")
			}()
			if err := s.a.DB.Transaction(func(tx *gorm.DB) error {
				if err := tx.Table("tasks").Where("id=?", job.ID).Updates(platform.Row{"status": "cancelled", "lease_generation": job.LeaseGeneration + 1, "lease_until": nil}).Error; err != nil {
					return err
				}
				return ForceStopConsole(tx, job.ID, s.a.Time())
			}); err != nil {
				t.Fatal(err)
			}
			close(completion)
			if err := <-done; err == nil {
				t.Fatal("late preparation write survived task cancellation")
			}
			var saved consoleSession
			if err := s.a.DB.Table("console_sessions").Where("id=?", row.ID).Take(&saved).Error; err != nil {
				t.Fatal(err)
			}
			if saved.Status != "closed" || len(saved.SecretsCiphertext) != 0 || saved.OperationPath != "/1.0/operations/retained" || saved.PrepareStartedAt != nil {
				t.Fatalf("cancelled console resurrected or lost retained operation evidence: %+v", saved)
			}
		})
	}
}

func TestConsoleForceStopCancelsPreconnectionAndOwnedBridge(t *testing.T) {
	s := &consoleService{a: platform.New(nil, platform.Config{}), bridges: make(map[string]*consoleBridge), prepareCancels: make(map[uint64]context.CancelFunc), bridgeTasks: make(map[uint64]*consoleBridge)}
	consoleServices.Store(s.a, s)
	defer consoleServices.Delete(s.a)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bridge, cleanup := consoleTestBridge()
	defer cleanup()
	other, cleanupOther := consoleTestBridge()
	defer cleanupOther()
	s.prepareCancels[10], s.bridgeTasks[10], s.bridgeTasks[11] = cancel, bridge, other
	CancelConsolePreparation(s.a, 10)
	select {
	case <-ctx.Done():
	default:
		t.Fatal("force stop left preconnection context live")
	}
	select {
	case <-bridge.done:
	default:
		t.Fatal("force stop left owned upstream channels live")
	}
	select {
	case <-other.done:
		t.Fatal("force stop crossed task ownership")
	default:
	}
}

func TestMySQLConsoleRevocationCancelsRemotelyStoppedPreconnection(t *testing.T) {
	s, row := consoleTestService(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bridge, cleanup := consoleTestBridge()
	defer cleanup()
	s.prepareCancels = map[uint64]context.CancelFunc{10: cancel}
	s.bridgeTasks = map[uint64]*consoleBridge{10: bridge}
	if err := s.a.DB.Table("console_sessions").Where("id=?", row.ID).Updates(platform.Row{"status": "preparing", "prepare_task_id": 10}).Error; err != nil {
		t.Fatal(err)
	}
	// No local after-commit callback: another backend committed the closure.
	if err := ForceStopConsole(s.a.DB, 10, s.a.Time()); err != nil {
		t.Fatal(err)
	}
	if err := s.revoke(context.Background(), s.a); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("remote force stop left preconnection context live")
	}
	select {
	case <-bridge.done:
	default:
		t.Fatal("remote force stop left preconnected channels live")
	}
}
