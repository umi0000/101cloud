package platform

import (
	"gorm.io/gorm"
	"time"
)

// TaskStopHook persists a consumer's domain revocation in the force-stop SQL
// transaction and optionally returns process-local cleanup for successful commit.
type TaskStopHook func(*gorm.DB, Row, time.Time) (func(), error)

func (a *App) RegisterTaskStopHook(kind string, hook TaskStopHook) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.taskStopHooks == nil {
		a.taskStopHooks = make(map[string]TaskStopHook)
	}
	if a.taskStopHooks[kind] != nil {
		panic("duplicate task stop hook: " + kind)
	}
	a.taskStopHooks[kind] = hook
}

func (a *App) StopTaskResources(tx *gorm.DB, task Row, now time.Time) (func(), error) {
	a.mu.RLock()
	hook := a.taskStopHooks[Text(task["task_type"])]
	a.mu.RUnlock()
	if hook == nil {
		return nil, nil
	}
	return hook(tx, task, now)
}
