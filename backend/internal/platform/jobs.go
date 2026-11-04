package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
)

type Job struct {
	ID              uint64
	Type            string
	UserID          uint64
	NodeID          uint64
	InstanceID      uint64
	Parameters      json.RawMessage
	Phase           string
	LeaseGeneration uint64
}
type JobHandler func(context.Context, *App, Job) error
type poller struct {
	name     string
	interval time.Duration
	run      func(context.Context, *App) error
}
type uncertain struct{ error }

func (e *uncertain) Unwrap() error { return e.error }

func Unknown(e error) error {
	if e == nil {
		e = errors.New("external result unknown")
	}
	return &uncertain{e}
}
func (a *App) RegisterJob(t string, h JobHandler) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.jobs[t] != nil {
		panic("duplicate job: " + t)
	}
	a.jobs[t] = h
}
func (a *App) RegisterReconciler(t string, h JobHandler) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.reconcilers == nil {
		a.reconcilers = map[string]JobHandler{}
	}
	a.reconcilers[t] = h
}
func (a *App) ReconcileJob(ctx context.Context, j Job) error {
	a.mu.RLock()
	h := a.reconcilers[j.Type]
	a.mu.RUnlock()
	if h == nil {
		return E(409, "invalid_state", "no safe reconciler for task")
	}
	return h(ctx, a, j)
}
func (a *App) AddPoller(name string, interval time.Duration, fn func(context.Context, *App) error) {
	if interval <= 0 {
		panic("invalid poller interval")
	}
	a.mu.Lock()
	a.pollers = append(a.pollers, poller{name, interval, fn})
	a.mu.Unlock()
}
func Enqueue(db *gorm.DB, kind string, userID, nodeID, instanceID uint64, mutation bool, params any) (Row, error) {
	b, e := json.Marshal(params)
	if e != nil {
		return nil, e
	}
	priority := 0
	if kind == "instance.stop" || kind == "instance.reconcile" {
		if p, ok := params.(Row); ok && strings.HasPrefix(Text(p["reason"]), "credit-enforcement:") {
			priority = 100
		}
	}
	r := Row{"task_type": kind, "idempotency_scope": "internal", "idempotency_key": RandomToken(), "user_id": nullableID(userID), "node_id": nullableID(nodeID), "instance_id": nullableID(instanceID), "is_instance_mutation": mutation, "status": "queued", "priority": priority, "parameters_redacted": string(b)}
	if e = db.Table("tasks").Create(&r).Error; e != nil {
		return nil, WrapDB(e)
	}
	id := Uint(r["id"])
	if id == 0 {
		id = Uint(r["@id"])
	}
	if id == 0 {
		return nil, errors.New("task insert did not return its identity")
	}
	return Row{"task_id": fmt.Sprint(id), "instance_id": nullableStringID(instanceID), "status": "queued"}, nil
}
func nullableStringID(v uint64) any {
	if v == 0 {
		return nil
	}
	return fmt.Sprint(v)
}
func (a *App) claim(ctx context.Context, owner string, urgent bool) (Job, error) {
	var job Job
	e := a.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var r Row
		condition := ""
		if urgent {
			condition = " AND priority>=100"
		}
		res := tx.Raw("SELECT * FROM tasks WHERE status='queued' AND (next_attempt_at IS NULL OR next_attempt_at<=?)"+condition+" ORDER BY priority DESC,created_at,id LIMIT 1 FOR UPDATE SKIP LOCKED", a.Time()).Scan(&r)
		if res.Error != nil {
			return res.Error
		}
		if Uint(r["id"]) == 0 {
			return gorm.ErrRecordNotFound
		}
		next := Uint(r["lease_generation"]) + 1
		now := a.Time()
		if e := tx.Table("tasks").Where("id=? AND status='queued'", r["id"]).Updates(Row{"status": "running", "lease_owner": owner, "lease_until": now.Add(90 * time.Second), "lease_generation": next, "attempt_count": gorm.Expr("attempt_count+1"), "started_at": now, "finished_at": nil, "revision": gorm.Expr("revision+1")}).Error; e != nil {
			return e
		}
		job = Job{ID: Uint(r["id"]), Type: Text(r["task_type"]), UserID: Uint(r["user_id"]), NodeID: Uint(r["node_id"]), InstanceID: Uint(r["instance_id"]), Parameters: json.RawMessage(Text(r["parameters_redacted"])), Phase: Text(r["phase"]), LeaseGeneration: next}
		return nil
	})
	return job, e
}
func (a *App) Step(ctx context.Context, j Job, key, operation, status string) error {
	return a.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var alive uint64
		if e := tx.Raw("SELECT id FROM tasks WHERE id=? AND lease_generation=? AND status='running' AND lease_until>? FOR UPDATE", j.ID, j.LeaseGeneration, a.Time()).Scan(&alive).Error; e != nil {
			return e
		}
		if alive != j.ID {
			return errors.New("lost task lease")
		}
		r := Row{"task_id": j.ID, "step_key": key, "attempt": j.LeaseGeneration, "status": status, "parameters_redacted": "{}", "started_at": a.Time()}
		if operation != "" {
			var n Row
			if e := tx.Table("nodes").Where("id=?", j.NodeID).Take(&n).Error; e != nil {
				return e
			}
			r["node_id"] = j.NodeID
			r["project_name"] = n["default_project_name"]
			r["operation_id"] = operation
		}
		if status == "succeeded" || status == "failed" || status == "cancelled" {
			r["finished_at"] = a.Time()
		}
		var old Row
		res := tx.Table("task_steps").Where("task_id=? AND step_key=? AND attempt=?", j.ID, key, j.LeaseGeneration).Take(&old)
		if errors.Is(res.Error, gorm.ErrRecordNotFound) {
			r["result_redacted"] = `{"native_dispatch_fenced":true}`
			if e := tx.Table("task_steps").Create(&r).Error; e != nil {
				return e
			}
		} else if res.Error != nil {
			return res.Error
		} else {
			r["started_at"] = old["started_at"]
			if e := tx.Table("task_steps").Where("id=?", old["id"]).Updates(r).Error; e != nil {
				return e
			}
		}
		return tx.Table("tasks").Where("id=? AND lease_generation=?", j.ID, j.LeaseGeneration).Update("phase", key).Error
	})
}
func (a *App) TaskResult(ctx context.Context, j Job, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	res := a.DB.WithContext(ctx).Table("tasks").Where("id=? AND lease_generation=? AND status='running' AND lease_until>?", j.ID, j.LeaseGeneration, a.Time()).Updates(Row{"result_redacted": string(b), "revision": gorm.Expr("revision+1")})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return errors.New("lost task lease")
	}
	return nil
}
func invokeJob(ctx context.Context, a *App, j Job, h JobHandler) (err error) {
	defer func() {
		if recover() != nil {
			err = Unknown(errors.New("task execution interrupted"))
		}
	}()
	if h == nil {
		return errors.New("unregistered task type")
	}
	return h(ctx, a, j)
}
func (a *App) execute(ctx context.Context, j Job, owner string) {
	a.mu.RLock()
	h := a.jobs[j.Type]
	a.mu.RUnlock()
	runCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	done := make(chan struct{})
	go func() {
		tick := time.NewTicker(20 * time.Second)
		defer tick.Stop()
		defer close(done)
		for {
			select {
			case <-runCtx.Done():
				return
			case <-tick.C:
				r := a.DB.WithContext(runCtx).Table("tasks").Where("id=? AND lease_owner=? AND lease_generation=? AND status='running'", j.ID, owner, j.LeaseGeneration).Update("lease_until", a.Time().Add(90*time.Second))
				if r.Error != nil || r.RowsAffected != 1 {
					cancel()
					return
				}
			}
		}
	}()
	e := invokeJob(runCtx, a, j, h)
	cancel()
	<-done
	finishCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	status := "succeeded"
	code := any(nil)
	var finished any = a.Time()
	if e != nil {
		var u *uncertain
		if errors.As(e, &u) || ctx.Err() != nil || errors.Is(e, context.DeadlineExceeded) || errors.Is(e, context.Canceled) {
			status = "unknown"
			finished = nil
		} else {
			status = "failed"
		}
		code = PublicError(e, status == "unknown")["code"]
		LogError("task "+j.Type, e)
	}
	if err := a.finishTask(finishCtx, j, owner, status, finished, code, e); err != nil {
		LogError("finish task", err)
	}
}

// finishTask commits task and open-step errors together under the same live lease.
func (a *App) finishTask(ctx context.Context, j Job, owner, status string, finished, code any, cause error) error {
	return a.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var task Row
		if err := tx.Raw("SELECT * FROM tasks WHERE id=? AND lease_owner=? AND lease_generation=? AND status='running' AND lease_until>? FOR UPDATE", j.ID, owner, j.LeaseGeneration, a.Time()).Scan(&task).Error; err != nil {
			return err
		}
		if Uint(task["id"]) != j.ID {
			return errors.New("lost task lease before completion")
		}
		changes := Row{"status": status, "finished_at": finished, "error_code": code, "lease_owner": nil, "lease_until": nil, "revision": gorm.Expr("revision+1")}
		if cause != nil {
			b, err := json.Marshal(PublicError(cause, status == "unknown"))
			if err != nil {
				return err
			}
			result := gorm.Expr("JSON_SET(COALESCE(result_redacted, JSON_OBJECT()), '$.error', JSON_EXTRACT(?, '$'))", string(b))
			changes["result_redacted"] = result
			if err = tx.Table("task_steps").Where("task_id=? AND attempt=? AND status IN ?", j.ID, j.LeaseGeneration, []string{"running", "waiting", "unknown"}).Updates(Row{"status": status, "finished_at": finished, "error_code": code, "result_redacted": result}).Error; err != nil {
				return err
			}
			// Exec validation may have already closed the active step as failed.
			// Fill its missing error without rewriting it to the task's unknown
			// status, or touching any older/different failed step.
			if err = tx.Table("task_steps").Where("task_id=? AND attempt=? AND step_key=? AND status='failed' AND error_code IS NULL AND JSON_EXTRACT(COALESCE(result_redacted, JSON_OBJECT()), '$.error') IS NULL", j.ID, j.LeaseGeneration, Text(task["phase"])).Updates(Row{"error_code": code, "result_redacted": result}).Error; err != nil {
				return err
			}
		} else {
			changes["result_redacted"] = gorm.Expr("JSON_REMOVE(COALESCE(result_redacted, JSON_OBJECT()), '$.error')")
		}
		return tx.Table("tasks").Where("id=?", j.ID).Updates(changes).Error
	})
}
func (a *App) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for worker := range 5 {
		wg.Add(1)
		go func(urgent bool) {
			defer wg.Done()
			owner := RandomToken()
			tick := time.NewTicker(time.Second)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					j, e := a.claim(ctx, owner, urgent)
					if errors.Is(e, gorm.ErrRecordNotFound) {
						continue
					}
					if e != nil {
						LogError("claim task", e)
						continue
					}
					a.execute(ctx, j, owner)
				}
			}
		}(worker == 4)
	}
	a.mu.RLock()
	ps := append([]poller(nil), a.pollers...)
	a.mu.RUnlock()
	ps = append(ps, poller{"lease-expiry", 15 * time.Second, func(ctx context.Context, a *App) error {
		return a.DB.WithContext(ctx).Table("tasks").Where("status='running' AND lease_until<?", a.Time()).Updates(Row{"status": "unknown", "error_code": "lease_expired", "lease_owner": nil, "lease_until": nil, "revision": gorm.Expr("revision+1")}).Error
	}})
	for _, p := range ps {
		wg.Add(1)
		go func(p poller) {
			defer wg.Done()
			tick := time.NewTicker(p.interval)
			defer tick.Stop()
			for {
				if e := p.run(ctx, a); e != nil && ctx.Err() == nil {
					LogError("poller "+p.name, e)
				}
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
				}
			}
		}(p)
	}
	wg.Wait()
	a.mu.RLock()
	hooks := append([]func(){}, a.shutdownHooks...)
	a.mu.RUnlock()
	for _, hook := range hooks {
		hook()
	}
}
