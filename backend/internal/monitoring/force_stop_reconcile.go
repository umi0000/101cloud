package monitoring

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"101cloud/backend/internal/platform"
	"gorm.io/gorm"
)

func (s *consoleService) reconcileForceStopped(ctx context.Context, a *platform.App, job platform.Job) error {
	var input struct {
		SourceTaskID json.Number `json:"source_task_id"`
	}
	if err := json.Unmarshal(job.Parameters, &input); err != nil {
		return platform.Unknown(err)
	}
	source, err := platform.Get(a.DB.WithContext(ctx), "tasks", platform.Uint(input.SourceTaskID), false)
	if err != nil {
		return platform.Unknown(err)
	}
	if platform.Text(source["task_type"]) != "monitoring.console.prepare" || platform.Text(source["status"]) != "cancelled" || platform.Text(source["error_code"]) != "task_force_stopped" || platform.Uint(source["node_id"]) != job.NodeID || platform.Uint(source["user_id"]) != job.UserID || platform.Uint(source["instance_id"]) != job.InstanceID {
		return platform.Unknown(errors.New("force-stopped console source identity changed"))
	}
	var args struct {
		SessionID string `json:"session_id"`
	}
	if err = json.Unmarshal([]byte(platform.Text(source["parameters_redacted"])), &args); err != nil {
		return platform.Unknown(err)
	}
	var session consoleSession
	if err = a.DB.WithContext(ctx).Table("console_sessions").Where("id=? AND prepare_task_id=?", args.SessionID, source["id"]).Take(&session).Error; err != nil {
		return platform.Unknown(err)
	}
	if session.Status != "closed" || session.NodeID != job.NodeID || session.InstanceID != job.InstanceID {
		return platform.Unknown(errors.New("force-stopped console session is not confirmed closed"))
	}
	if err = a.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error { return consoleRecoveryFence(tx, a, job, source, session) }); err != nil {
		return err
	}
	var steps []platform.Row
	if err = a.DB.WithContext(ctx).Table("task_steps").Where("task_id=?", source["id"]).Find(&steps).Error; err != nil {
		return err
	}
	type operation struct{ path, project string }
	operations := make(map[operation]struct{})
	project := ""
	add := func(reference, recordedProject string) error {
		if reference == "" {
			return nil
		}
		if !strings.HasPrefix(reference, "/") {
			reference = "/1.0/operations/" + reference
		}
		u, err := url.Parse(reference)
		if err != nil || u.IsAbs() || u.Host != "" || u.Fragment != "" || u.RawPath != "" {
			return errors.New("invalid recorded console operation")
		}
		path, err := consoleOperationPath(u.Path)
		if err != nil {
			return err
		}
		query, err := url.ParseQuery(u.RawQuery)
		if err != nil || len(query) > 1 || len(query["project"]) > 1 || len(query) == 1 && len(query["project"]) != 1 {
			return errors.New("invalid recorded console operation project")
		}
		if nativeProject := query.Get("project"); nativeProject != "" {
			if recordedProject != "" && recordedProject != nativeProject {
				return errors.New("recorded console operation project changed")
			}
			recordedProject = nativeProject
		}
		if recordedProject == "" {
			if project == "" {
				instance, err := platform.Get(a.DB.WithContext(ctx), "instances", job.InstanceID, false)
				if err != nil {
					return err
				}
				if platform.Uint(instance["node_id"]) != job.NodeID {
					return errors.New("console instance node changed")
				}
				project = platform.Text(instance["project_name"])
			}
			recordedProject = project
		}
		if recordedProject == "" {
			return errors.New("console operation project is unavailable")
		}
		operations[operation{path, recordedProject}] = struct{}{}
		return nil
	}
	dispatched := false
	for _, step := range steps {
		var result struct {
			NativeDispatchStarted bool `json:"native_dispatch_started"`
		}
		if encoded := platform.Text(step["result_redacted"]); encoded != "" {
			if err = json.Unmarshal([]byte(encoded), &result); err != nil {
				return platform.Unknown(err)
			}
		}
		dispatched = dispatched || result.NativeDispatchStarted
		if result.NativeDispatchStarted && platform.Text(step["operation_id"]) == "" {
			return platform.Unknown(errors.New("console dispatch has no recorded operation identity; external outcome remains unknown"))
		}
		if platform.Uint(step["node_id"]) != 0 && platform.Uint(step["node_id"]) != job.NodeID {
			return platform.Unknown(errors.New("recorded console operation node changed"))
		}
		if err = add(platform.Text(step["operation_id"]), platform.Text(step["project_name"])); err != nil {
			return platform.Unknown(err)
		}
	}
	if err = add(session.OperationPath, ""); err != nil {
		return platform.Unknown(err)
	}
	if len(operations) == 0 {
		var result struct {
			ForceStop struct {
				ExternalState string `json:"external_state"`
			} `json:"force_stop"`
		}
		if err = json.Unmarshal([]byte(platform.Text(source["result_redacted"])), &result); err != nil || dispatched || result.ForceStop.ExternalState != "not_dispatched" {
			return platform.Unknown(errors.New("console dispatch outcome remains unknown; no recorded operation is available for read-only reconciliation"))
		}
	}
	for op := range operations {
		response, err := a.Agent(ctx, job.NodeID, http.MethodGet, op.path+"?project="+url.QueryEscape(op.project), nil)
		if err != nil || platform.Uint(response["error_code"]) != 0 {
			return platform.Unknown(errors.New("recorded console operation is not observable; terminal outcome remains unknown"))
		}
		metadata := consoleOperation(response)
		if id := platform.Text(metadata["id"]); id != "" && "/1.0/operations/"+id != op.path {
			return platform.Unknown(errors.New("observed console operation identity changed"))
		}
		switch platform.Uint(metadata["status_code"]) {
		case 200, 400, 401:
		default:
			return platform.Unknown(errors.New("recorded console operation is not confirmed terminal"))
		}
	}
	if err = a.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := consoleRecoveryFence(tx, a, job, source, session); err != nil {
			return err
		}
		return tx.Table("tasks").Where("id=?", source["id"]).Updates(platform.Row{"result_redacted": gorm.Expr("JSON_SET(COALESCE(result_redacted, JSON_OBJECT()), '$.force_stop.reconciled_at', ?, '$.force_stop.reconciliation_completed', CAST('true' AS JSON))", a.Time().Format(time.RFC3339Nano)), "revision": gorm.Expr("revision+1")}).Error
	}); err != nil {
		return err
	}
	return a.TaskResult(ctx, job, platform.Row{"source_task_id": platform.Text(source["id"]), "reconciled": true, "session_id": session.ID, "status": "closed"})
}

func consoleRecoveryFence(tx *gorm.DB, a *platform.App, job platform.Job, source platform.Row, session consoleSession) error {
	var wrapper platform.Row
	if err := tx.Raw("SELECT id,node_id,user_id,instance_id FROM tasks WHERE id=? AND lease_generation=? AND status='running' AND lease_until>? FOR UPDATE", job.ID, job.LeaseGeneration, a.Time()).Scan(&wrapper).Error; err != nil {
		return err
	}
	if platform.Uint(wrapper["id"]) != job.ID || platform.Uint(wrapper["node_id"]) != job.NodeID || platform.Uint(wrapper["user_id"]) != job.UserID || platform.Uint(wrapper["instance_id"]) != job.InstanceID {
		return platform.Unknown(errors.New("console reconciliation lost its original live lease"))
	}
	var current platform.Row
	if err := tx.Raw("SELECT id,status,error_code,lease_generation,revision FROM tasks WHERE id=? FOR UPDATE", source["id"]).Scan(&current).Error; err != nil {
		return err
	}
	if platform.Text(current["status"]) != "cancelled" || platform.Text(current["error_code"]) != "task_force_stopped" || platform.Uint(current["lease_generation"]) != platform.Uint(source["lease_generation"]) || platform.Uint(current["revision"]) != platform.Uint(source["revision"]) {
		return platform.Unknown(errors.New("force-stopped console reconciliation source changed"))
	}
	var closed consoleSession
	if err := tx.Raw("SELECT * FROM console_sessions WHERE id=? FOR UPDATE", session.ID).Scan(&closed).Error; err != nil {
		return err
	}
	if closed.Status != "closed" || closed.PrepareTaskID == nil || *closed.PrepareTaskID != platform.Uint(source["id"]) || closed.Revision != session.Revision {
		return platform.Unknown(errors.New("closed console session changed during reconciliation"))
	}
	return nil
}
