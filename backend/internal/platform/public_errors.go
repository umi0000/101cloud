package platform

import (
	"encoding/json"
	"errors"
)

// PublicError serializes only errors deliberately constructed for public use.
// In particular, an uncertain wrapper does not erase its safe underlying cause.
func PublicError(err error, unknown bool) Row {
	var he *HTTPError
	if errors.As(err, &he) && he.Code != "" && he.Message != "" {
		var details map[string]any
		if b, e := json.Marshal(he.Details); e == nil {
			_ = json.Unmarshal(b, &details)
		}
		return Row{"code": he.Code, "message": he.Message, "details": details}
	}
	if unknown {
		return Row{"code": "external_result_unknown", "message": "操作结果暂时无法确认，请勿重复提交；请联系管理员核对节点状态并执行任务对账。", "details": nil}
	}
	return Row{"code": "operation_failed", "message": "任务未完成，请查看实例状态并联系管理员排查；确认失败原因已解决后再提交新请求。", "details": nil}
}

// StoredTaskError never exposes arbitrary result fields or historical raw errors.
func StoredTaskError(result any, status, code string) Row {
	if status != "failed" && status != "unknown" {
		return nil
	}
	var saved struct {
		Error *struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(Text(result)), &saved) == nil && saved.Error != nil && saved.Error.Code != "" && saved.Error.Code == code && saved.Error.Message != "" {
		return Row{"code": saved.Error.Code, "message": saved.Error.Message, "details": saved.Error.Details}
	}
	fallback := PublicError(nil, status == "unknown")
	if code != "" {
		fallback["code"] = code
	}
	return fallback
}

func requestFieldError(field, message string) error {
	details := Row{"action": "请检查对应字段的名称、类型和允许范围后重新提交。"}
	// A field name is diagnostic, never a license to echo arbitrary request data.
	safe := field != "" && len(field) <= 128
	for _, r := range field {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '.' || r == '[' || r == ']') {
			safe = false
			break
		}
	}
	if safe {
		details["field"] = field
	}
	return EDetails(422, "validation_failed", message, details)
}
