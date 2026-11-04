package instances

import (
	"net/http"
	"testing"
	"time"

	"101cloud/backend/internal/platform"
)

func TestCreationProgressFrozenOptionalStages(t *testing.T) {
	instance := platform.Row{"incus_config_snapshot": `{"creation":{"gpu_count":0,"cloud_init":false}}`}
	progress := creationProgressView(platform.Row{"status": "running"}, instance, nil)
	stages := progress["stages"].([]platform.Row)
	for _, index := range []int{2, 4, 5} {
		if stages[index]["status"] != "skipped" {
			t.Fatalf("optional stage %v was not skipped: %v", index, stages[index])
		}
	}
	legacy := creationProgressView(platform.Row{}, platform.Row{}, nil)["stages"].([]platform.Row)
	if legacy[2]["status"] != "pending" || legacy[4]["status"] != "pending" {
		t.Fatal("missing frozen evidence was presented as skipped")
	}
}

func TestCreationProgressStoppedIsNotCompleted(t *testing.T) {
	steps := []platform.Row{}
	for _, key := range []string{"create.preflight", "create.native", "state.start", "create.verify"} {
		steps = append(steps, platform.Row{"step_key": key, "status": "succeeded", "started_at": time.Now()})
	}
	instance := platform.Row{"incus_config_snapshot": `{"creation":{"gpu_count":0,"cloud_init":false}}`}
	complete := creationProgressView(platform.Row{"status": "succeeded"}, instance, steps)
	if platform.Bool(complete["initialization_pending"]) {
		t.Fatal("fully evidenced creation remains pending")
	}
	stopped := creationProgressView(platform.Row{"status": "succeeded", "result_redacted": `{"initialization":"pending"}`}, instance, steps)
	if !platform.Bool(stopped["initialization_pending"]) || stopped["stages"].([]platform.Row)[6]["status"] == "succeeded" {
		t.Fatal("stopped initialization was presented as all green")
	}
}

func TestCreationProgressFailedExecAndUnknown(t *testing.T) {
	instance := platform.Row{"init_template_version_id": 1}
	steps := []platform.Row{{"step_key": "init.template", "status": "failed"}, {"step_key": "create.native", "status": "waiting"}}
	progress := creationProgressView(platform.Row{"status": "unknown"}, instance, steps)
	stages := progress["stages"].([]platform.Row)
	if stages[5]["status"] != "failed" || stages[1]["status"] != "unknown" || !platform.Bool(progress["initialization_pending"]) {
		t.Fatalf("uncertain evidence became success: %v", progress)
	}
}

func TestCreationExecValidationBeforeSuccess(t *testing.T) {
	for _, value := range []platform.Row{{}, {"metadata": platform.Row{"return": "bad"}}, {"metadata": platform.Row{"return": 1}}} {
		if err := workerExit(value); err == nil {
			t.Fatal("unverified or unsuccessful exec was accepted")
		}
	}
	if err := workerExit(platform.Row{"metadata": platform.Row{"return": 0}}); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLCreationProgressReadOnlyAPI(t *testing.T) {
	f := newPlanConfigurationFixture(t)
	created := f.call(false, http.MethodPost, "/instances", f.createBody("1", "2.5"), 0, http.StatusAccepted, "")
	id := platform.Text(created["instance_id"])
	taskID := platform.Text(created["task_id"])
	for range 2 {
		task := f.call(false, http.MethodGet, "/tasks/"+taskID, nil, 0, http.StatusOK, "")
		progress := object(task["creation_progress"])
		if !platform.Bool(progress["initialization_pending"]) || len(rows(progress["stages"])) != 7 {
			t.Fatalf("queued creation lost progress: %v", task)
		}
		instance := f.call(false, http.MethodGet, "/instances/"+id, nil, 0, http.StatusOK, "")
		if platform.Text(instance["creation_task_id"]) != taskID || instance["login_mode"] != "none" {
			t.Fatalf("instance recovery summary missing: %v", instance)
		}
		if _, leaked := instance["login_config_ciphertext"]; leaked {
			t.Fatal("sensitive login configuration escaped DTO")
		}
	}
	if f.count("tasks") != 1 || f.count("task_steps") != 0 || f.reads.Load() != 1 {
		t.Fatal("refresh dispatched work or created progress journals")
	}
	f.requestUserID = 1
	f.call(false, http.MethodGet, "/tasks/"+taskID, nil, 0, http.StatusNotFound, "")
}

func TestCreationProgressCloudSubstepsPreserveUncertainty(t *testing.T) {
	for _, status := range []string{"failed", "unknown", "waiting", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			steps := []platform.Row{
				{"step_key": "login.cloud-ready", "status": status, "task_status": "running"},
				{"step_key": "init.cloud-ready", "status": "succeeded", "finished_at": time.Now()},
			}
			progress := creationProgressView(platform.Row{"status": "running"}, platform.Row{}, steps)
			stage := progress["stages"].([]platform.Row)[4]
			want := status
			if status == "waiting" {
				want = "running"
			} else if status == "cancelled" {
				want = "pending"
			}
			if stage["status"] != want || !platform.Bool(progress["initialization_pending"]) {
				t.Fatalf("successful sibling concealed %s: %v", status, progress)
			}
		})
	}
}

func TestCreationProgressUsesRecoveryStepOwnerStatus(t *testing.T) {
	steps := []platform.Row{{"step_key": "create.native", "status": "waiting", "task_status": "running"}}
	progress := creationProgressView(platform.Row{"status": "unknown"}, platform.Row{}, steps)
	if progress["stages"].([]platform.Row)[1]["status"] != "running" {
		t.Fatal("historical task uncertainty overrode live reconciliation")
	}
	steps = append(steps, platform.Row{"step_key": "create.native", "status": "succeeded", "task_status": "succeeded"})
	progress = creationProgressView(platform.Row{"status": "unknown"}, platform.Row{}, steps)
	if progress["stages"].([]platform.Row)[1]["status"] != "succeeded" {
		t.Fatal("historical task uncertainty overrode confirmed reconciliation")
	}
}
