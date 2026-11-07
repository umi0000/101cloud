package monitoring

import (
	"context"
	"sync"
	"time"

	"101cloud/backend/internal/platform"
	"gorm.io/gorm"
)

var consoleServices sync.Map // *platform.App -> *consoleService

// ForceStopConsole closes preparation-owned sessions in the task cancellation
// transaction. Native operations are retained; no operation DELETE is issued.
func ForceStopConsole(tx *gorm.DB, taskID uint64, now time.Time) error {
	return tx.Table("console_sessions").Where("prepare_task_id=? AND status IN ?", taskID, []string{"preparing", "ready", "active"}).Updates(platform.Row{"status": "closed", "closed_at": now, "secrets_ciphertext": nil, "revision": gorm.Expr("revision+1")}).Error
}

// CancelConsolePreparation runs only after cancellation commits. It interrupts
// pending native I/O and closes preconnected channels owned by this process.
func CancelConsolePreparation(a *platform.App, taskID uint64) {
	value, ok := consoleServices.Load(a)
	if !ok {
		return
	}
	value.(*consoleService).cancelPreparation(taskID)
}

func (s *consoleService) cancelPreparation(taskID uint64) {
	s.mu.Lock()
	cancel := s.prepareCancels[taskID]
	bridge := s.bridgeTasks[taskID]
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if bridge != nil {
		bridge.close(1008, "会话已撤销")
	}
}

func (s *consoleService) prepareUpdate(ctx context.Context, job platform.Job, id string, changes platform.Row, first bool) error {
	return s.a.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var task platform.Row
		if err := tx.Raw("SELECT id FROM tasks WHERE id=? AND lease_generation=? AND status='running' AND lease_until>? FOR UPDATE", job.ID, job.LeaseGeneration, s.a.Time()).Scan(&task).Error; err != nil {
			return err
		}
		if platform.Uint(task["id"]) != job.ID {
			return platform.E(409, "task_lease_lost", "console preparation no longer owns its live lease")
		}
		q := tx.Table("console_sessions").Where("id=? AND prepare_task_id=? AND status='preparing'", id, job.ID)
		if first {
			q = q.Where("prepare_started_at IS NULL")
		}
		result := q.Updates(changes)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return platform.Unknown(platform.E(409, "console_preparation_interrupted", "console preparation was interrupted; upstream operation must not be replayed"))
		}
		return nil
	})
}
