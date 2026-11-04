package instances

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"101cloud/backend/internal/platform"
)

func TestMySQLInitializationTemplateValidationDoesNotDispatch(t *testing.T) {
	for _, reason := range []string{"template_status_mismatch", "template_hash_mismatch", "template_timeout_invalid"} {
		t.Run(reason, func(t *testing.T) {
			db, a, job, r, mutations := creationCleanupDB(t)
			for _, query := range []string{
				"CREATE TEMPORARY TABLE instance_init_runs (instance_id BIGINT,init_template_version_id BIGINT,status VARCHAR(24))",
				"CREATE TEMPORARY TABLE init_template_versions (id BIGINT PRIMARY KEY,status VARCHAR(24),script_content TEXT,script_sha256 VARCHAR(64),timeout_seconds BIGINT)",
			} {
				if err := db.Exec(query).Error; err != nil {
					t.Fatal(err)
				}
			}
			script := "echo secret-template-content"
			sum := sha256.Sum256([]byte(script))
			template := platform.Row{"id": 1, "status": "active", "script_content": script, "script_sha256": hex.EncodeToString(sum[:]), "timeout_seconds": 60}
			switch reason {
			case "template_status_mismatch":
				template["status"] = "draft"
			case "template_hash_mismatch":
				template["script_sha256"] = strings.Repeat("0", 64)
			case "template_timeout_invalid":
				template["timeout_seconds"] = 0
			}
			if err := db.Table("init_template_versions").Create(&template).Error; err != nil {
				t.Fatal(err)
			}
			r["init_template_version_id"] = 1
			err := workerInitialize(t.Context(), a, job, r)
			if err == nil || mutations.Load() != 0 {
				t.Fatal("invalid template executed or lacked diagnostic")
			}
			public := platform.PublicError(err, true)
			details := public["details"].(map[string]any)
			if public["code"] != "initialization_script_failed" || details["stage"] != "init.template" || details["reason"] != reason {
				t.Fatalf("template diagnostic lost: %+v", public)
			}
			encoded, _ := json.Marshal(public)
			if strings.Contains(string(encoded), script) || strings.Contains(string(encoded), platform.Text(template["script_sha256"])) {
				t.Fatal("template content or hash exposed")
			}
		})
	}
}

func TestMySQLHistoricalScriptExitKeepsOriginalMeaningAndNeverReplays(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/wait") {
			t.Errorf("historical failed script was redispatched: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(platform.Row{"metadata": platform.Row{"status_code": 200, "metadata": platform.Row{"return": 58}}})
	})
	_, a, job, _, _ := creationCleanupDB(t, handler)
	if err := a.Step(t.Context(), job, "init.template", "/1.0/operations/old-script", "waiting"); err != nil {
		t.Fatal(err)
	}
	_, err := workerOperation(t.Context(), a, job, "init.template", func() (map[string]any, error) {
		t.Fatal("old script must not be resubmitted")
		return nil, nil
	}, nil)
	if err == nil {
		t.Fatal("historical failed script accepted")
	}
	public := platform.PublicError(err, true)
	details := public["details"].(map[string]any)
	if public["code"] != "initialization_script_failed" || details["reason"] != "script_exit_nonzero" || platform.Text(details["exit_code"]) != "58" {
		t.Fatalf("old exit code was interpreted as diagnostic protocol: %+v", public)
	}
	step, err := workerStep(t.Context(), a, job, "init.template")
	if err != nil || platform.Text(step["status"]) != "failed" {
		t.Fatal("failed script lost its terminal step state")
	}
}
