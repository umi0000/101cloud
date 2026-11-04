package instances

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"101cloud/backend/internal/platform"
)

// CloudInitDiagnosticScript is a fixed, read-only guest probe. Its only output is
// an allowlisted exit code: 32 + status index*8 + reason index. Raw status JSON
// never leaves the guest, and is never recorded by the exec API.
// It is deliberately separate from the original wait operation: old journals
// retain their original exit-code meaning and can never authorize a replay.
const CloudInitDiagnosticScript = `import json, os, shutil, subprocess, sys
statuses = ("not_run", "running", "done", "degraded", "error", "disabled", "unknown")
status = "unknown"
reason = 5
if not os.path.isfile("/usr/bin/cloud-init"):
    sys.exit(32 + 6*8 + 3)
try:
    result = subprocess.run(["/usr/bin/cloud-init", "status", "--format", "json"], stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=8, check=False)
    if len(result.stdout) > 1048576:
        raise ValueError("oversized status")
    data = json.loads(result.stdout)
    status = data.get("status", "unknown")
    if status in ("not run", "not started"):
        status = "not_run"
    extended = data.get("extended_status", "")
    if status not in statuses:
        status = "unknown"
    if isinstance(extended, str) and "degraded" in extended and status == "done":
        status = "degraded"
    errors = data.get("errors", [])
    recoverable = data.get("recoverable_errors", {})
    if errors:
        reason = 1
    elif status == "error":
        reason = 1
    elif status == "degraded" or (status == "done" and recoverable):
        reason = 0
        if status == "done":
            status = "degraded"
    else:
        reason = 6
    # Require both a current-boot warning and the missing executable.
    warning = False
    def host_warning(text):
        return isinstance(text, str) and ("Failed generating key type" in text or "Failed to generate ssh host keys" in text)
    if isinstance(recoverable, dict):
        warning = any(host_warning(item) for items in recoverable.values() if isinstance(items, list) for item in items)
    if isinstance(errors, list):
        warning = warning or any(host_warning(item) for item in errors)
    if reason in (0, 1) and warning and shutil.which("ssh-keygen") is None:
        reason = 2
    elif reason == 6 and status not in ("running", "unknown") and (not os.path.isfile("/var/lib/cloud/instance/boot-finished") or (sys.argv[1:] == ["login"] and not os.path.isfile("/etc/101cloud/ssh-login-mode"))):
        reason = 4
except (OSError, ValueError, TypeError, AttributeError, subprocess.SubprocessError):
    status, reason = "unknown", 5
sys.exit(32 + statuses.index(status)*8 + reason)
`

var cloudInitDiagnosticStatuses = [...]string{"not_run", "running", "done", "degraded", "error", "disabled", "unknown"}

func initializationStage(key string) bool {
	return key == "login.cloud-ready" || key == "init.cloud-ready" || key == "init.template"
}

func initializationError(stage, code, reason, status string, exit *int64) error {
	details := platform.Row{"stage": stage, "reason": reason, "cloud_init_status": status}
	message, action := "初始化执行失败。", "请管理员检查对应初始化阶段；系统未采集脚本标准错误，请在实例内安全检查后处理原任务，勿重复执行初始化。"
	switch code {
	case "initialization_cloud_init_degraded":
		message, action = "cloud-init 已完成但报告降级，初始化尚未通过。", "请管理员检查 cloud-init 的可恢复错误；完成标记不代表所有模块成功，请修复镜像配置后处理原任务，勿重跑 cloud-init。"
	case "initialization_cloud_init_failed":
		message, action = "cloud-init 报告初始化错误。", "请管理员在实例内检查 cloud-init 错误并修复镜像或配置；勿重复执行初始化。"
	case "initialization_tool_missing":
		message, action = "初始化所需工具缺失。", "请管理员检查镜像内所需工具及软件包；修复后核对原任务，勿重复执行初始化。"
	case "initialization_marker_missing":
		message, action = "未找到初始化完成标记。", "请管理员检查 cloud-init 是否启用及首次启动是否完整；勿仅凭进程退出成功放行或重跑初始化。"
	case "initialization_timeout":
		message, action = "初始化等待超时，执行结果仍需确认。", "请管理员核对原生操作和实例当前状态；超时不代表脚本已停止，勿重复执行初始化。"
	case "initialization_result_unknown":
		message, action = "初始化执行结果无法确认。", "请管理员核对原操作记录和实例状态；系统保留资源且不会自动重放初始化。"
	case "initialization_diagnostic_unavailable":
		message, action = "初始化失败，暂时无法获取安全诊断。", "请管理员检查实例内 cloud-init 状态和诊断工具；系统未采集原始日志，勿重复执行初始化。"
	}
	switch reason {
	case "template_hash_mismatch":
		message, action = "初始化模板完整性校验失败，未执行脚本。", "请管理员核对已发布模板内容与摘要；不要绕过完整性校验或重复派发。"
	case "template_status_mismatch":
		message, action = "初始化模板未发布，未执行脚本。", "请管理员检查实例冻结的初始化模板发布状态，不要直接重放初始化。"
	case "template_timeout_invalid":
		message, action = "初始化模板缺少有效超时配置，未执行脚本。", "请管理员为初始化模板配置有效超时并核对原任务后处理。"
	}
	details["action"] = action
	if exit != nil {
		details["exit_code"] = *exit
	}
	return platform.EDetails(http.StatusConflict, code, message, details)
}

func initializationOperationError(ctx context.Context, stage string, cause error) error {
	if cause == nil || errors.Is(cause, workerSkipped) {
		return cause
	}
	var safe *platform.HTTPError
	if errors.As(cause, &safe) && strings.HasPrefix(safe.Code, "initialization_") {
		return cause
	}
	code, reason := "initialization_result_unknown", "result_unknown"
	if errors.Is(cause, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		code, reason = "initialization_timeout", "timeout"
	}
	return platform.Unknown(initializationError(stage, code, reason, "unknown", nil))
}

func initializationExit(result platform.Row) (int64, bool) {
	value, ok := object(result["metadata"])["return"]
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(platform.Text(value), 10, 32)
	return n, err == nil && n >= 0 && n <= 255
}

func initializationTerminatedError(stage string, original int64) error {
	err := initializationError(stage, "initialization_script_failed", "command_terminated", "unknown", &original)
	api := err.(*platform.HTTPError)
	api.Message = "初始化命令已被终止，终止原因尚未确认。"
	api.Details.(platform.Row)["action"] = "请管理员核对原操作是否超时、被外部终止或触及资源限制；系统未采集标准错误，勿重复执行初始化。"
	return err
}

func cloudInitDiagnosticError(stage string, original int64, diagnostic int64) error {
	if original == 124 {
		return initializationError(stage, "initialization_timeout", "timeout", "unknown", &original)
	}
	if original == 137 {
		return initializationTerminatedError(stage, original)
	}
	status, reason := "unknown", int64(5)
	if diagnostic >= 32 && diagnostic < 32+int64(len(cloudInitDiagnosticStatuses))*8 {
		status = cloudInitDiagnosticStatuses[(diagnostic-32)/8]
		reason = (diagnostic - 32) % 8
	}
	code, category := "initialization_diagnostic_unavailable", "diagnostic_unavailable"
	switch reason {
	case 0:
		code, category = "initialization_cloud_init_degraded", "cloud_init_degraded"
	case 1:
		code, category = "initialization_cloud_init_failed", "cloud_init_errors"
	case 2:
		code, category = "initialization_tool_missing", "ssh_host_key_generation_missing_tool"
	case 3:
		code, category = "initialization_tool_missing", "required_tool_missing"
	case 4:
		code, category = "initialization_marker_missing", "completion_marker_missing"
	case 6:
		code, category = "initialization_result_unknown", "result_unknown"
	}
	err := initializationError(stage, code, category, status, &original)
	if reason == 2 || reason == 3 {
		details := err.(*platform.HTTPError).Details.(platform.Row)
		if reason == 2 {
			err.(*platform.HTTPError).Message = "缺少 ssh-keygen 导致 SSH 主机密钥生成失败。"
			details["missing_tool"], details["host_key_generation_failed"] = "ssh-keygen", true
			details["action"] = "请管理员为镜像安装提供 ssh-keygen 的 OpenSSH 软件包，并核对 SSH host-key 生成警告；系统不会重跑 cloud-init，请安全处理原实例和任务。"
		} else {
			details["missing_tool"] = "cloud-init"
		}
	}
	return err
}

func workerCloudInitDiagnostic(ctx context.Context, a *platform.App, j platform.Job, stage string, original int64) error {
	if original == 124 || original == 137 {
		return cloudInitDiagnosticError(stage, original, -1)
	}
	probe, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	r, err := workerInstance(probe, a, j)
	if err != nil {
		return cloudInitDiagnosticError(stage, original, -1)
	}
	if _, _, err = workerObserve(probe, a, r); err != nil {
		return cloudInitDiagnosticError(stage, original, -1)
	}
	mode := "template"
	if stage == "login.cloud-ready" {
		mode = "login"
	}
	// A recovery may run a fresh read-only diagnostic, never replay a prior exec.
	// Give each probe its own fenced journal rather than reusing its dispatch marker.
	key := stage + ".diagnostic." + platform.RandomToken()[:12]
	if err := a.Step(probe, j, key, "", "running"); err != nil {
		return cloudInitDiagnosticError(stage, original, -1)
	}
	v, err := a.Agent(platform.WithTaskDispatch(probe, j, key), j.NodeID, http.MethodPost, workerPath(r, "/exec"), platform.Row{
		"command":     []string{"/usr/bin/timeout", "--signal=KILL", "15", "/usr/bin/python3", "-c", CloudInitDiagnosticScript, mode},
		"environment": platform.Row{"PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
		"interactive": false, "wait-for-websocket": false, "record-output": false,
	})
	if err != nil || platform.Text(v["operation"]) == "" {
		return cloudInitDiagnosticError(stage, original, -1)
	}
	operation := platform.Text(v["operation"])
	if ref, parseErr := url.Parse(operation); parseErr == nil {
		q := ref.Query()
		if q.Get("project") == "" {
			q.Set("project", platform.Text(r["project_name"]))
			ref.RawQuery = q.Encode()
			operation = ref.String()
		}
	}
	v, err = a.WaitOperation(probe, j.NodeID, operation)
	if err != nil {
		return cloudInitDiagnosticError(stage, original, -1)
	}
	m, err := workerMetadata(v)
	if err != nil {
		return cloudInitDiagnosticError(stage, original, -1)
	}
	if err := a.Step(probe, j, key, operation, "succeeded"); err != nil {
		return cloudInitDiagnosticError(stage, original, -1)
	}
	code, ok := initializationExit(m)
	if !ok {
		code = -1
	}
	return cloudInitDiagnosticError(stage, original, code)
}
