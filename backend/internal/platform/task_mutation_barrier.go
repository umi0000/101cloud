package platform

import (
	"encoding/json"
	"strings"

	"gorm.io/gorm"
)

// A control task can mutate a guest without owning the ordinary mutation slot
// (credit shutdown deliberately bypasses busy-instance admission). Its stopped
// external work must still fence unrelated future mutations.
const RetainedTaskMutationSQL = "status='cancelled' AND error_code='task_force_stopped' AND (is_instance_mutation=1 OR JSON_UNQUOTE(JSON_EXTRACT(result_redacted,'$.force_stop.instance_mutation_barrier'))='true') AND COALESCE(JSON_UNQUOTE(JSON_EXTRACT(result_redacted,'$.force_stop.reconciliation_completed')),'false')<>'true'"

func TaskCanResolveMutationBarrier(task Row) bool {
	return taskCanResolveMutationBarrier(Text(task["task_type"]), []byte(Text(task["parameters_redacted"])))
}

func taskCanResolveMutationBarrier(kind string, parameters []byte) bool {
	var input struct {
		Cleanup  bool   `json:"cleanup_stuck"`
		ReadOnly bool   `json:"read_only_force_stop_reconcile"`
		Reason   string `json:"reason"`
	}
	if json.Unmarshal(parameters, &input) != nil {
		return false
	}
	return (kind == "instance.delete" && input.Cleanup) || (kind == "instance.reconcile" && (input.ReadOnly || strings.HasPrefix(input.Reason, "credit-enforcement:"))) || (kind == "instance.stop" && strings.HasPrefix(input.Reason, "credit-enforcement:"))
}

func TaskMutationBarrier(db *gorm.DB, instanceID uint64) error {
	if instanceID == 0 {
		return nil
	}
	var count int64
	if err := db.Table("tasks").Where("instance_id=? AND ("+RetainedTaskMutationSQL+")", instanceID).Count(&count).Error; err != nil {
		return err
	}
	if count != 0 {
		return E(409, "task_reconciliation_required", "已停止编排的任务仍保留资源安全屏障，请管理员核对外部操作后再操作实例。")
	}
	return nil
}
