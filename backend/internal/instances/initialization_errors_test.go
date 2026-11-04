package instances

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"101cloud/backend/internal/platform"
)

func TestCloudInitDiagnosticClassification(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is required to exercise the fixed guest diagnostic")
	}
	for _, tc := range []struct {
		name, status, extended, warning, code, reason         string
		errors, missingKey, marker, missingCloud, invalidJSON bool
	}{
		{name: "arch-host-key-evidence", status: "done", extended: "degraded done", warning: "Failed generating key type rsa", missingKey: true, marker: true, code: "initialization_tool_missing", reason: "ssh_host_key_generation_missing_tool"},
		{name: "missing-tool-without-warning", status: "done", extended: "degraded done", missingKey: true, marker: true, code: "initialization_cloud_init_degraded", reason: "cloud_init_degraded"},
		{name: "warning-with-tool-installed", status: "done", extended: "degraded done", warning: "Failed generating key type rsa", marker: true, code: "initialization_cloud_init_degraded", reason: "cloud_init_degraded"},
		{name: "fatal-errors", status: "error", errors: true, marker: true, code: "initialization_cloud_init_failed", reason: "cloud_init_errors"},
		{name: "marker-missing", status: "done", code: "initialization_marker_missing", reason: "completion_marker_missing"},
		{name: "disabled", status: "disabled", code: "initialization_marker_missing", reason: "completion_marker_missing"},
		{name: "not-started", status: "not started", code: "initialization_marker_missing", reason: "completion_marker_missing"},
		{name: "cloud-init-missing", status: "unknown", missingCloud: true, code: "initialization_tool_missing", reason: "required_tool_missing"},
		{name: "running", status: "running", marker: true, code: "initialization_result_unknown", reason: "result_unknown"},
		{name: "running-with-warning", status: "running", warning: "Failed generating key type rsa", missingKey: true, code: "initialization_result_unknown", reason: "result_unknown"},
		{name: "unknown", status: "future-status", marker: true, code: "initialization_result_unknown", reason: "result_unknown"},
		{name: "invalid-json", status: "done", marker: true, invalidJSON: true, code: "initialization_diagnostic_unavailable", reason: "diagnostic_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := platform.Row{"status": tc.status, "extended_status": tc.extended, "errors": []string{}, "recoverable_errors": platform.Row{}, "detail": "password=secret $6$hash cloud-init.user-data"}
			if tc.errors {
				data["errors"] = []string{"raw-error-with-private-data"}
			}
			if tc.warning != "" {
				data["recoverable_errors"] = platform.Row{"WARNING": []string{tc.warning + " private-log-value"}}
			}
			fixture, _ := json.Marshal(platform.Row{"data": data, "missing_key": tc.missingKey, "marker": tc.marker, "missing_cloud": tc.missingCloud, "invalid_json": tc.invalidJSON})
			setup := "import json, os, shutil, subprocess, sys, types\nfixture=json.loads(" + strconv.Quote(string(fixture)) + ")\n" +
				"def isfile(path):\n    if path == '/usr/bin/cloud-init': return not fixture['missing_cloud']\n    return fixture['marker']\n" +
				"os.path.isfile=isfile\nshutil.which=lambda name: None if fixture['missing_key'] else '/usr/bin/ssh-keygen'\n" +
				"def run(command, **kwargs):\n    assert command == ['/usr/bin/cloud-init', 'status', '--format', 'json']\n    return types.SimpleNamespace(stdout=b'raw-invalid-private-data' if fixture['invalid_json'] else json.dumps(fixture['data']).encode())\nsubprocess.run=run\nsys.argv=['probe','login']\n"
			cmd := exec.CommandContext(t.Context(), python, "-c", setup+CloudInitDiagnosticScript)
			output, runErr := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(runErr, &exit) || len(output) != 0 {
				t.Fatalf("probe must emit only finite exit status: %v %q", runErr, output)
			}
			public := platform.PublicError(platform.Unknown(cloudInitDiagnosticError("login.cloud-ready", 2, int64(exit.ExitCode()))), true)
			details := public["details"].(map[string]any)
			if public["code"] != tc.code || details["reason"] != tc.reason || platform.Text(details["exit_code"]) != "2" {
				t.Fatalf("wrong classification: %+v", public)
			}
			encoded, _ := json.Marshal(public)
			for _, secret := range []string{"private-log-value", "raw-error-with-private-data", "$6$hash", "password=secret", "cloud-init.user-data"} {
				if strings.Contains(string(encoded), secret) {
					t.Fatalf("unsafe diagnostic persisted %q", secret)
				}
			}
		})
	}
}

func TestInitializationExitAndDiagnosticUnavailable(t *testing.T) {
	for _, value := range []any{nil, "invalid-secret", -1, 256, 1.5} {
		if _, known := initializationExit(platform.Row{"metadata": platform.Row{"return": value}}); known {
			t.Fatalf("invalid exit status accepted: %v", value)
		}
	}
	for _, diagnostic := range []int64{-1, 0, 1, 127, 137, 255} {
		public := platform.PublicError(cloudInitDiagnosticError("init.cloud-ready", 2, diagnostic), true)
		if public["code"] != "initialization_diagnostic_unavailable" || platform.Text(public["details"].(map[string]any)["exit_code"]) != "2" {
			t.Fatalf("probe failure overwrote original exit: %+v", public)
		}
	}
	for _, tc := range []struct {
		exit         int64
		code, reason string
	}{
		{124, "initialization_timeout", "timeout"},
		{137, "initialization_script_failed", "command_terminated"},
	} {
		public := platform.PublicError(cloudInitDiagnosticError("login.cloud-ready", tc.exit, 58), true)
		if public["code"] != tc.code || public["details"].(map[string]any)["reason"] != tc.reason {
			t.Fatalf("termination semantics lost: %+v", public)
		}
	}
}

func TestInitializationOperationErrorSafety(t *testing.T) {
	for _, tc := range []struct {
		cause error
		code  string
	}{
		{errors.New("stderr password=$6$secret INIT_PARAMETERS_JSON=private"), "initialization_result_unknown"},
		{context.DeadlineExceeded, "initialization_timeout"},
		{platform.E(502, "node_unavailable", "upstream-private-data"), "initialization_result_unknown"},
	} {
		public := platform.PublicError(initializationOperationError(t.Context(), "init.template", tc.cause), true)
		encoded, _ := json.Marshal(public)
		if public["code"] != tc.code || strings.Contains(string(encoded), "private") || strings.Contains(string(encoded), "$6$") {
			t.Fatalf("unsafe or generic operation error: %s", encoded)
		}
	}
}
