package instances

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"101cloud/backend/internal/billing"
	"101cloud/backend/internal/platform"
	"gorm.io/gorm"
)

// Only the verified cleanup worker may pass authority into nested stop/delete
// operations; queued user jobs cannot inherit retirement privileges.
type creationCleanupAuthorityKey struct{}

// A failure is retirement, not deletion. The marker, desired-state fence and
// financial cancellation commit together; physical reclamation has its own journal.
func runCreate(ctx context.Context, a *platform.App, j platform.Job) (cause error) {
	defer func() {
		if recover() != nil {
			cause = platform.Unknown(errors.New("creation execution interrupted"))
		}
		if cause == nil {
			return
		}
		finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := markFailedCreation(finish, a, j, cause, false); err != nil {
			platform.LogError("retire failed creation", err)
		}
	}()
	return runCreateWork(ctx, a, j)
}

func creationDelivered(db *gorm.DB, r platform.Row) (bool, error) {
	if platform.Bool(object(r["incus_config_snapshot"])["creation_delivered"]) {
		return true, nil
	}
	var count int64
	err := db.Table("tasks t").Where("t.instance_id=? AND ((t.task_type='instance.create' AND t.status='succeeded') OR EXISTS (SELECT 1 FROM task_steps s WHERE s.task_id=t.id AND s.step_key='create.verify' AND s.status='succeeded'))", r["id"]).Count(&count).Error
	return count != 0, err
}

func confirmedCreationFailure(cause error) error {
	public := platform.PublicError(cause, true)
	details := object(public["details"])
	if details == nil {
		details = map[string]any{}
	}
	details["creation_failure_confirmed"] = true
	return platform.EDetails(409, platform.Text(public["code"]), platform.Text(public["message"]), details)
}

func creationDefiniteError(cause error) bool {
	if cause == nil {
		return false
	}
	details := object(platform.PublicError(cause, true)["details"])
	if platform.Bool(details["creation_failure_confirmed"]) {
		return true
	}
	switch platform.Text(details["reason"]) {
	case "script_exit_nonzero", "cloud_init_errors", "ssh_host_key_generation_missing_tool", "required_tool_missing", "completion_marker_missing", "cloud_init_degraded", "command_terminated", "template_status_mismatch", "template_hash_mismatch", "template_timeout_invalid":
		return true
	}
	return false
}

func markFailedCreation(ctx context.Context, a *platform.App, j platform.Job, cause error, definite bool) error {
	if j.Type != "instance.create" {
		return nil
	}
	return a.DB.WithContext(ctx).Transaction(func(db *gorm.DB) error {
		if err := lockOwnerAccount(db, j.UserID); err != nil {
			return err
		}
		r, err := platform.Get(db, "instances", j.InstanceID, true)
		if err != nil {
			return err
		}
		if platform.Uint(r["user_id"]) != j.UserID || platform.Uint(r["node_id"]) != j.NodeID {
			return errors.New("failed creation ownership changed")
		}
		if r["deleted_at"] != nil {
			return nil
		}
		if r["creation_failed_at"] != nil {
			return nil
		}
		delivered, err := creationDelivered(db, r)
		if err != nil || delivered {
			return err
		}
		var sources []platform.Row
		if err = locked(db).Table("tasks").Where("instance_id=? AND task_type='instance.create'", j.InstanceID).Order("id").Find(&sources).Error; err != nil {
			return err
		}
		if len(sources) != 1 {
			return nil
		}
		source := sources[0]
		if platform.Text(source["status"]) == "cancelled" {
			return nil
		}
		if platform.Uint(source["user_id"]) != j.UserID || platform.Uint(source["node_id"]) != j.NodeID {
			return errors.New("original creation ownership changed")
		}
		if platform.Text(source["status"]) == "running" || platform.Uint(source["id"]) != j.ID {
			if err = workerLease(a, db, j); err != nil {
				return err
			}
		}
		// A reconciliation wrapper cannot retire an unrelated or currently live source.
		if platform.Uint(source["id"]) != j.ID && platform.Text(source["status"]) == "running" {
			return nil
		}
		var failedSteps int64
		if err = db.Table("task_steps").Where("task_id=? AND status='failed' AND step_key IN ?", source["id"], []string{"init.template", "login.cloud-ready", "init.cloud-ready"}).Count(&failedSteps).Error; err != nil {
			return err
		}
		releasedPreflight := platform.Text(r["provisioning_state"]) == "failed" && platform.Text(r["incus_uuid"]) == "" && platform.Bool(object(r["incus_config_snapshot"])["creation_reservations_released"])
		known := definite || creationDefiniteError(cause) || failedSteps > 0 || releasedPreflight
		if !known {
			return nil
		}
		now := a.Time()
		state := "pending"
		if r["deleted_at"] != nil {
			state = "completed"
		}
		changes := platform.Row{"creation_failed_at": now, "creation_failure_task_id": source["id"], "creation_cleanup_state": state, "desired_state": "deleted", "provisioning_state": "failed", "revision": gorm.Expr("revision+1")}
		if err = db.Table("instances").Where("id=? AND creation_failed_at IS NULL", r["id"]).Updates(changes).Error; err != nil {
			return err
		}
		r["creation_failed_at"] = now
		r["creation_failure_task_id"] = source["id"]
		if err = billing.CancelFailedCreation(ctx, db, r, now); err != nil {
			return err
		}
		taskChanges := platform.Row{"is_instance_mutation": false, "revision": gorm.Expr("revision+1")}
		if platform.Text(source["status"]) != "running" {
			taskChanges["status"] = "failed"
			taskChanges["finished_at"] = now
		}
		return db.Table("tasks").Where("id=?", source["id"]).Updates(taskChanges).Error
	})
}

// discovery is deliberately limited to never-delivered failed create tasks. A
// lost response, disconnected node or UI timeout is not proof of failed creation.
func pollFailedCreations(ctx context.Context, a *platform.App) error {
	var candidates []platform.Row
	if err := a.DB.WithContext(ctx).Table("tasks t").Select("t.*").Joins("JOIN instances i ON i.id=t.instance_id").Where("t.task_type='instance.create' AND t.status IN ('failed','unknown') AND i.creation_failed_at IS NULL AND i.deleted_at IS NULL AND i.desired_state<>'deleted' AND NOT EXISTS (SELECT 1 FROM tasks delivered WHERE delivered.instance_id=i.id AND delivered.task_type='instance.create' AND delivered.status='succeeded')").Order("t.id").Find(&candidates).Error; err != nil {
		return err
	}
	var failures []error
	for _, source := range candidates {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		job := platform.Job{ID: platform.Uint(source["id"]), Type: "instance.create", InstanceID: platform.Uint(source["instance_id"]), UserID: platform.Uint(source["user_id"]), NodeID: platform.Uint(source["node_id"]), LeaseGeneration: platform.Uint(source["lease_generation"])}
		definite := false
		cause := platform.StoredTaskError(source["result_redacted"], platform.Text(source["status"]), platform.Text(source["error_code"]))
		var safeCause error
		if cause != nil {
			safeCause = platform.EDetails(409, platform.Text(cause["code"]), platform.Text(cause["message"]), platform.Row(object(cause["details"])))
		}
		if !definite && !creationDefiniteError(safeCause) {
			var steps []platform.Row
			if err := a.DB.WithContext(ctx).Table("task_steps").Where("task_id=? AND (status='failed' OR operation_id IS NOT NULL)", job.ID).Find(&steps).Error; err != nil {
				failures = append(failures, err)
				continue
			}
			for _, step := range steps {
				if initializationStage(platform.Text(step["step_key"])) && platform.Text(step["status"]) == "failed" {
					definite = true
					break
				}
				op := platform.Text(step["operation_id"])
				if op == "" {
					continue
				}
				probe, cancel := context.WithTimeout(ctx, 5*time.Second)
				v, err := a.Agent(probe, job.NodeID, http.MethodGet, op, nil)
				cancel()
				if err != nil {
					continue
				}
				m, err := workerMetadata(v)
				if err != nil {
					continue
				}
				code := platform.Uint(m["status_code"])
				if code >= 400 {
					definite = true
				}
				if initializationStage(platform.Text(step["step_key"])) {
					if exit, known := initializationExit(m); known && exit != 0 && code >= 200 {
						definite = true
					}
				}
			}
		}
		if err := markFailedCreation(ctx, a, job, safeCause, definite); err != nil {
			failures = append(failures, err)
		}
	}
	var retired []platform.Row
	if err := a.DB.WithContext(ctx).Table("instances").Where("creation_failed_at IS NOT NULL AND creation_cleanup_state<>'completed'").Order("id").Find(&retired).Error; err != nil {
		return err
	}
	for _, r := range retired {
		err := a.DB.WithContext(ctx).Transaction(func(db *gorm.DB) error {
			if err := lockOwnerAccount(db, platform.Uint(r["user_id"])); err != nil {
				return err
			}
			current, err := platform.Get(db, "instances", platform.Uint(r["id"]), true)
			if err != nil {
				return err
			}
			var explicitCleanup int64
			if err := db.Table("tasks").Where("instance_id=? AND task_type='instance.delete' AND JSON_EXTRACT(parameters_redacted,'$.cleanup_stuck')=true", r["id"]).Count(&explicitCleanup).Error; err != nil || explicitCleanup != 0 || current["deleted_at"] != nil {
				return err
			}
			if platform.Text(current["creation_cleanup_state"]) == "completed" {
				return nil
			}
			// Do not run alongside a live/queued mutation, including an administrator's
			// reconciliation wrapper. The old unknown mutation slot is handed over only
			// after all of its external operations have become quiescent.
			var active int64
			if err = db.Table("tasks").Where("instance_id=? AND status IN ('queued','running','waiting')", r["id"]).Count(&active).Error; err != nil || active != 0 {
				return err
			}
			var cleanup []platform.Row
			if err = locked(db).Table("tasks").Where("instance_id=? AND task_type='instance.creation_cleanup'", r["id"]).Order("id DESC").Limit(1).Find(&cleanup).Error; err != nil {
				return err
			}
			if len(cleanup) > 0 {
				task := cleanup[0]
				if platform.Text(task["status"]) == "succeeded" {
					return nil
				}
				// Same task identity and journal: unknown mutations are observed, not sent again.
				return db.Table("tasks").Where("id=? AND status IN ('failed','unknown')", task["id"]).Updates(platform.Row{"status": "queued", "lease_owner": nil, "lease_until": nil, "revision": gorm.Expr("revision+1")}).Error
			}
			_, err = platform.Enqueue(db, "instance.creation_cleanup", platform.Uint(r["user_id"]), platform.Uint(r["node_id"]), platform.Uint(r["id"]), true, platform.Row{"force": true})
			return err
		})
		if err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// No new mutation may race a late create/start/exec. Ambiguous dispatch without
// an operation ID cannot be repaired by replaying it or trusting a single 404.
func creationQuiescent(ctx context.Context, a *platform.App, j platform.Job) error {
	instance, err := workerInstance(ctx, a, j)
	if err != nil {
		return err
	}
	var steps []platform.Row
	if err := a.DB.WithContext(ctx).Table("task_steps s").Select("s.*").Joins("JOIN tasks t ON t.id=s.task_id").Where("t.instance_id=? AND t.task_type<>'instance.creation_cleanup'", j.InstanceID).Find(&steps).Error; err != nil {
		return err
	}
	for _, step := range steps {
		key, status, op := platform.Text(step["step_key"]), platform.Text(step["status"]), platform.Text(step["operation_id"])
		if key == "create.preflight" || key == "create.verify" || key == "delete.verify_reclamation" || key == "delete.finalize" {
			continue
		}
		if op == "" {
			if status == "succeeded" || status == "cancelled" {
				continue
			}
			return errors.New("original mutation has ambiguous dispatch; retain resources")
		}
		// GPU configuration journals store an operation UUID, unlike native
		// lifecycle journals. Resolve it in the frozen instance project.
		if key == "gpu.configure" && !strings.HasPrefix(op, "/1.0/operations/") {
			op = "/1.0/operations/" + url.PathEscape(op) + "?project=" + url.QueryEscape(platform.Text(instance["project_name"]))
		}
		v, err := a.Agent(ctx, j.NodeID, http.MethodGet, op, nil)
		if err != nil {
			if platform.Text(v["type"]) != "error" || platform.Uint(v["error_code"]) != 404 {
				return err
			}
			// Successful journals and failed initialization journals (recorded only after
			// validating an exec exit) survive operation retention expiry. A 404 without
			// this terminal evidence cannot authorize reclamation; project enumeration
			// and post-delete GPU evidence are still required below.
			if status == "succeeded" || (initializationStage(key) && status == "failed") {
				continue
			}
			return err
		}
		m, err := workerMetadata(v)
		if err != nil {
			return err
		}
		if platform.Uint(m["status_code"]) < 200 {
			return errors.New("original operation is not terminal")
		}
	}
	return nil
}

func runCreationCleanup(ctx context.Context, a *platform.App, j platform.Job) (cause error) {
	authorized := false
	defer func() {
		if recover() != nil {
			cause = platform.Unknown(errors.New("creation cleanup interrupted"))
		}
		if !authorized {
			return
		}
		state := "completed"
		if cause != nil {
			state = "unknown"
		}
		err := a.DB.WithContext(context.WithoutCancel(ctx)).Transaction(func(db *gorm.DB) error {
			if err := lockWorkerAccount(db, a, j, j.UserID); err != nil {
				return err
			}
			return db.Table("instances").Where("id=? AND creation_failed_at IS NOT NULL", j.InstanceID).Update("creation_cleanup_state", state).Error
		})
		if err != nil && cause == nil {
			cause = platform.Unknown(err)
		}
	}()
	r, err := workerInstance(ctx, a, j)
	if err != nil {
		return err
	}
	if r["creation_failed_at"] == nil || platform.Text(r["desired_state"]) != "deleted" {
		return errors.New("cleanup requires retired failed creation")
	}
	authorized = true
	delivered, err := creationDelivered(a.DB.WithContext(ctx), r)
	if err != nil {
		return err
	}
	if delivered {
		return errors.New("delivered instance cannot be creation-cleaned")
	}
	if err = a.DB.WithContext(ctx).Transaction(func(db *gorm.DB) error {
		if err := lockWorkerAccount(db, a, j, j.UserID); err != nil {
			return err
		}
		return db.Table("instances").Where("id=?", j.InstanceID).Update("creation_cleanup_state", "cleaning").Error
	}); err != nil {
		return err
	}
	if r["deleted_at"] != nil {
		return nil
	}
	if err = creationQuiescent(ctx, a, j); err != nil {
		return platform.Unknown(err)
	}
	// UUID may not have reached SQL before the original worker crashed. Capture it
	// only after exact platform marker + approved project/name have been verified.
	absent, err := workerAbsent(ctx, a, r)
	if err != nil {
		return platform.Unknown(err)
	}
	if !absent {
		m, _, err := workerObserve(ctx, a, r)
		if err != nil {
			return platform.Unknown(err)
		}
		cfg := object(m["expanded_config"])
		if len(cfg) == 0 {
			cfg = object(m["config"])
		}
		if platform.Text(cfg["user.101cloud.instance_id"]) != platform.Text(r["id"]) {
			return platform.Unknown(errors.New("creation cleanup ownership marker mismatch"))
		}
		if platform.Text(r["incus_uuid"]) == "" {
			if err = a.DB.WithContext(ctx).Transaction(func(db *gorm.DB) error {
				if err := lockWorkerAccount(db, a, j, j.UserID); err != nil {
					return err
				}
				result := db.Table("instances").Where("id=? AND (incus_uuid IS NULL OR incus_uuid='')", r["id"]).Update("incus_uuid", cfg["volatile.uuid"])
				if result.Error != nil {
					return result.Error
				}
				if result.RowsAffected != 1 {
					return errors.New("creation cleanup UUID changed concurrently")
				}
				return nil
			}); err != nil {
				return err
			}
		}
	} else {
		// Quiescence plus a complete project enumeration establishes absence. Record
		// a delete intent before reusing the normal post-delete GPU reclamation path.
		if err = a.Step(ctx, j, "delete.native", "", "succeeded"); err != nil {
			return err
		}
	}
	j.Type = "instance.delete"
	j.Parameters = json.RawMessage(`{"force":true}`)
	return runDelete(context.WithValue(ctx, creationCleanupAuthorityKey{}, j.ID), a, j)
}
