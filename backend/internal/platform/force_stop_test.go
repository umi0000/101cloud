package platform

import (
	"context"
	"errors"
	"testing"
)

func TestCancelTaskExecutionsTargetsEveryAttempt(t *testing.T) {
	a := New(nil, Config{})
	first, _, unregisterFirst := a.registerTaskExecution(t.Context(), Job{ID: 1, LeaseGeneration: 1})
	defer unregisterFirst()
	second, _, unregisterSecond := a.registerTaskExecution(t.Context(), Job{ID: 1, LeaseGeneration: 2})
	defer unregisterSecond()
	other, _, unregisterOther := a.registerTaskExecution(t.Context(), Job{ID: 2})
	defer unregisterOther()
	a.CancelTaskExecutions(1, 999)
	for _, ctx := range []context.Context{first, second} {
		if !errors.Is(ctx.Err(), context.Canceled) || !ctx.Value(taskExecutionKey{}).(*taskExecution).forceStopping.Load() {
			t.Fatal("force-stop did not interrupt and mark every local attempt")
		}
	}
	if other.Err() != nil {
		t.Fatal("force-stop interrupted another task")
	}
	unregisterFirst()
	a.CancelTaskExecutions(1)
	late, _, unregisterLate := a.registerTaskExecution(t.Context(), Job{ID: 1, LeaseGeneration: 3})
	defer unregisterLate()
	a.CancelTaskExecutions(1)
	if late.Err() == nil {
		t.Fatal("postcommit cancellation missed a newly registered execution")
	}
}

func TestTaskBarrierResolutionAuthorityIsNarrow(t *testing.T) {
	for _, tc := range []struct {
		kind, parameters string
		allowed          bool
	}{
		{"instance.stop", `{"reason":"credit-enforcement:1"}`, true},
		{"instance.start", `{"reason":"credit-enforcement:1"}`, false},
		{"instance.restart", `{"cleanup_stuck":true}`, false},
		{"instance.delete", `{"cleanup_stuck":true}`, true},
		{"instance.reconcile", `{"read_only_force_stop_reconcile":true}`, true},
		{"instance.reconcile", `{"reason":"credit-enforcement:1"}`, true},
		{"instance.reconcile", `{}`, false},
		{"monitoring.console.prepare", `{"read_only_force_stop_reconcile":true}`, false},
	} {
		if got := TaskCanResolveMutationBarrier(Row{"task_type": tc.kind, "parameters_redacted": tc.parameters}); got != tc.allowed {
			t.Fatalf("%s %s: got %t", tc.kind, tc.parameters, got)
		}
	}
}
