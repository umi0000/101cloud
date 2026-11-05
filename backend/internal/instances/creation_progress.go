package instances

import (
	"101cloud/backend/internal/platform"
	"gorm.io/gorm"
)

// Creation progress is a read-only projection of the journal and the instance's
// frozen configuration. Missing evidence never authorizes replay or success.
func creationProgress(db *gorm.DB, task platform.Row) (platform.Row, error) {
	instance, err := platform.Get(db, "instances", platform.Uint(task["instance_id"]), false)
	if err != nil {
		return nil, err
	}
	var tasks []platform.Row
	if err = db.Table("tasks").Select("id,parameters_redacted").Where("instance_id=? AND task_type='instance.reconcile'", task["instance_id"]).Order("id").Find(&tasks).Error; err != nil {
		return nil, err
	}
	ids := []uint64{platform.Uint(task["id"])}
	included := map[uint64]bool{ids[0]: true}
	for _, child := range tasks {
		if included[platform.Uint(object(child["parameters_redacted"])["source_task_id"])] {
			id := platform.Uint(child["id"])
			included[id] = true
			ids = append(ids, id)
		}
	}
	var steps []platform.Row
	if err = db.Table("task_steps s").Select("s.*,t.status AS task_status").Joins("JOIN tasks t ON t.id=s.task_id").Where("s.task_id IN ?", ids).Order("s.id").Find(&steps).Error; err != nil {
		return nil, err
	}
	return creationProgressView(task, instance, steps), nil
}

func creationProgressView(task, instance platform.Row, steps []platform.Row) platform.Row {
	latest := map[string]platform.Row{}
	for _, step := range steps {
		latest[platform.Text(step["step_key"])] = step
	}
	creation := object(object(instance["incus_config_snapshot"])["creation"])
	_, hasGPU := creation["gpu_count"]
	_, hasCloud := creation["cloud_init"]
	definitions := []struct {
		key      string
		journal  []string
		optional bool
	}{
		{"preflight", []string{"create.preflight"}, false},
		{"native", []string{"create.native"}, false},
		{"gpu", []string{"gpu.configure"}, hasGPU && platform.Uint(creation["gpu_count"]) == 0},
		{"start", []string{cloudInitPrepareStep, "state.start"}, false},
		{"cloud_init", []string{"login.cloud-ready", "init.cloud-ready"}, hasCloud && !platform.Bool(creation["cloud_init"])},
		{"script", []string{"init.template"}, platform.Uint(instance["init_template_version_id"]) == 0},
		{"verify", []string{"create.verify"}, false},
	}
	stages := make([]platform.Row, 0, len(definitions))
	pending := false
	for _, definition := range definitions {
		stage := platform.Row{"key": definition.key, "status": "pending"}
		if definition.optional {
			stage["status"] = "skipped"
		}
		found := false
		for _, key := range definition.journal {
			if step := latest[key]; step != nil {
				status := platform.Text(step["status"])
				switch status {
				case "waiting":
					status = "running"
				case "cancelled":
					status = "pending"
				case "pending", "running", "succeeded", "failed", "unknown":
				default:
					status = "unknown"
				}
				ownerStatus := platform.Text(step["task_status"])
				if ownerStatus == "" {
					ownerStatus = platform.Text(task["status"])
				}
				if status == "running" && (ownerStatus == "unknown" || ownerStatus == "failed") {
					status = ownerStatus
				}
				if !found || creationStagePriority(status) > creationStagePriority(platform.Text(stage["status"])) {
					stage["status"] = status
				}
				found = true
				started := platform.Stamp(step["started_at"])
				if !started.IsZero() && (platform.Stamp(stage["started_at"]).IsZero() || started.Before(platform.Stamp(stage["started_at"]))) {
					stage["started_at"] = step["started_at"]
				}
				finished := platform.Stamp(step["finished_at"])
				if !finished.IsZero() && finished.After(platform.Stamp(stage["finished_at"])) {
					stage["finished_at"] = step["finished_at"]
				}
			}
		}
		if definition.key == "start" && latest["state.start"] == nil && stage["status"] == "succeeded" {
			stage["status"] = "pending"
		}
		if stage["status"] == "running" || stage["status"] == "pending" || stage["status"] == "unknown" {
			delete(stage, "finished_at")
		}
		if stage["status"] != "succeeded" && stage["status"] != "skipped" {
			pending = true
		}
		stages = append(stages, stage)
	}
	if platform.Text(object(task["result_redacted"])["initialization"]) == "pending" {
		pending = true
		stages[len(stages)-1]["status"] = "pending"
	}
	return platform.Row{"stages": stages, "initialization_pending": pending}
}

// A successful substep cannot conceal an unsuccessful sibling substep.
func creationStagePriority(status string) int {
	switch status {
	case "failed":
		return 5
	case "unknown":
		return 4
	case "running":
		return 3
	case "pending":
		return 2
	case "succeeded":
		return 1
	}
	return 0
}
