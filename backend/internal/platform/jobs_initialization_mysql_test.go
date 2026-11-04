package platform

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestMySQLClosedActiveFailedStepReceivesErrorWithoutRewritingHistory(t *testing.T) {
	db := jobTestDB(t)
	queued, err := Enqueue(db, "regression.closed-exec", 0, 0, 0, false, Row{})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := IDString(Text(queued["task_id"]))
	a := New(db, Config{})
	job := Job{ID: id, LeaseGeneration: 2}
	if err = db.Table("tasks").Where("id=?", id).Updates(Row{"status": "running", "lease_owner": "owner", "lease_generation": 2, "lease_until": a.Time().Add(time.Minute)}).Error; err != nil {
		t.Fatal(err)
	}
	for _, old := range []Row{
		{"task_id": id, "step_key": "init.template", "attempt": 1, "status": "failed", "parameters_redacted": "{}", "result_redacted": `{"historical":"old-attempt"}`, "started_at": a.Time(), "finished_at": a.Time()},
		{"task_id": id, "step_key": "old.failure", "attempt": 2, "status": "failed", "parameters_redacted": "{}", "result_redacted": `{"historical":"other-step"}`, "started_at": a.Time(), "finished_at": a.Time()},
	} {
		if err = db.Table("task_steps").Create(&old).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err = a.Step(t.Context(), job, "init.template", "", "failed"); err != nil {
		t.Fatal(err)
	}
	cause := Unknown(EDetails(409, "initialization_script_failed", "初始化脚本退出失败。", Row{"stage": "init.template", "reason": "script_exit_nonzero", "exit_code": 23, "cloud_init_status": "unknown", "action": "请管理员安全检查脚本，不要重放。"}))
	if err = a.finishTask(context.Background(), job, "owner", "unknown", nil, "initialization_script_failed", cause); err != nil {
		t.Fatal(err)
	}
	var steps []Row
	if err = db.Table("task_steps").Where("task_id=?", id).Find(&steps).Error; err != nil {
		t.Fatal(err)
	}
	if len(steps) != 3 {
		t.Fatalf("expected all three current/historical steps, got %d", len(steps))
	}
	for _, step := range steps {
		var result map[string]any
		if err := json.Unmarshal([]byte(Text(step["result_redacted"])), &result); err != nil {
			t.Fatal(err)
		}
		if Uint(step["attempt"]) == 2 && Text(step["step_key"]) == "init.template" {
			storedError, ok := result["error"].(map[string]any)
			if Text(step["status"]) != "failed" || Text(step["error_code"]) != "initialization_script_failed" || !ok || Text(storedError["code"]) != "initialization_script_failed" || Stamp(step["finished_at"]).IsZero() {
				t.Fatalf("closed failed step lost its persisted error or terminal state: %+v", step)
			}
		} else {
			expected := "other-step"
			if Uint(step["attempt"]) == 1 && Text(step["step_key"]) == "init.template" {
				expected = "old-attempt"
			}
			if Text(step["error_code"]) != "" || Text(step["status"]) != "failed" || len(result) != 1 || Text(result["historical"]) != expected {
				t.Fatalf("unrelated historical failure changed: %+v", step)
			}
		}
	}
}
