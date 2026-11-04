package instances

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"

	"101cloud/backend/internal/platform"
)

func TestMySQLCreationCleanupResolvesLegacyGPUOperationInProject(t *testing.T) {
	const operation = "6f75e7c8-7d9c-47f2-bd84-618f77c05b0e"
	var observed atomic.Bool
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/1.0/operations/"+operation || r.URL.Query().Get("project") != "legacy-project" {
			t.Errorf("unexpected legacy operation probe: %s %s", r.Method, r.URL)
			http.Error(w, "unexpected probe", 400)
			return
		}
		observed.Store(true)
		_ = json.NewEncoder(w).Encode(platform.Row{"metadata": platform.Row{"status_code": 200}})
	})
	db, a, job, _, _ := creationCleanupDB(t, handler)
	if err := db.Table("instances").Where("id=?", job.InstanceID).Update("project_name", "legacy-project").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Table("task_steps").Create(&platform.Row{"task_id": job.ID, "step_key": "gpu.configure", "attempt": 1, "status": "waiting", "operation_id": operation, "parameters_redacted": "{}"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := creationQuiescent(t.Context(), a, job); err != nil {
		t.Fatal(err)
	}
	if !observed.Load() {
		t.Fatal("original GPU operation was not confirmed")
	}
}
