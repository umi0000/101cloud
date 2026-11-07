import type { Task } from '#/api/types';

import { reactive } from 'vue';
import { createPinia, setActivePinia } from 'pinia';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { ApiError } from '#/api/request';
import { useTaskStore } from './tasks';

const runtime = vi.hoisted(() => ({
  auth: null as null | { me: { user: { id: string; role: string } } },
  memberTask: vi.fn(), adminTask: vi.fn(), memberStop: vi.fn(), adminStop: vi.fn(),
  success: vi.fn(), error: vi.fn(), warning: vi.fn(),
}));
vi.mock('./auth', () => ({ useAuthStore: () => runtime.auth }));
vi.mock('#/api/backend', () => ({
  memberApi: { task: runtime.memberTask, forceStopTask: runtime.memberStop },
  opsApi: { task: runtime.adminTask, forceStopTask: runtime.adminStop },
}));
vi.mock('ant-design-vue', () => ({ notification: { success: runtime.success, error: runtime.error, warning: runtime.warning } }));
vi.mock('#/components/error-feedback.vue', () => ({ default: () => null }));

function task(overrides: Partial<Task> = {}): Task {
  return {
    id: 'task-a', instance_id: null, user_id: 'user-a', type: 'image.prepare', status: 'running',
    created_at: '2026-11-07T00:00:00Z', started_at: null, finished_at: null,
    error_code: null, phase: 'dispatch', revision: '7', next_action: 'wait', node_id: null, steps: [],
    ...overrides,
  };
}
const response = (value: Task) => ({ data: value, etag: `"rev-${value.revision}"`, requestId: 'request-a' });
const stopped = () => task({ status: 'cancelled', revision: '8', error_code: 'task_force_stopped', force_stop: { orchestration_stopped: true, external_state: 'requires_reconciliation', requires_reconciliation: true } });
async function flush() { for (let i = 0; i < 8; i++) await Promise.resolve(); }

beforeEach(() => {
  vi.useFakeTimers();
  vi.resetAllMocks();
  setActivePinia(createPinia());
  runtime.auth = reactive({ me: { user: { id: 'user-a', role: 'user' } } });
  runtime.memberTask.mockResolvedValue(response(task()));
  runtime.adminTask.mockResolvedValue(response(task({ user_id: 'another-owner' })));
  runtime.memberStop.mockResolvedValue(response(stopped()));
  runtime.adminStop.mockResolvedValue(response(stopped()));
});
afterEach(() => { useTaskStore().$reset(); vi.useRealTimers(); });

describe('task force-stop lifecycle', () => {
  it.each(['queued', 'running', 'unknown', 'waiting'] as const)('stops a %s task with its version, preserving history and refreshing its parent', async (status) => {
    runtime.memberTask.mockResolvedValueOnce(response(task({ status })));
    const done = vi.fn();
    const store = useTaskStore(); store.track('task-a', '镜像准备', done); await flush();
    runtime.memberTask.mockResolvedValue(response(stopped()));
    expect(await store.forceStop('task-a')).toBe(true);
    await flush();
    expect(runtime.memberStop).toHaveBeenCalledWith('task-a', { ifMatch: '"rev-7"', key: expect.any(String) });
    expect(runtime.adminStop).not.toHaveBeenCalled();
    expect(store.tracked).toHaveLength(1);
    expect(store.tracked[0]!.task?.force_stop?.requires_reconciliation).toBe(true);
    expect(store.active).toHaveLength(0);
    expect(done).toHaveBeenCalledTimes(1);
    expect(runtime.memberTask).toHaveBeenCalledTimes(2);
  });

  it('uses the administrator endpoint for another owner without a type whitelist', async () => {
    runtime.auth!.me.user.role = 'admin';
    const store = useTaskStore(); store.track('task-a', '系统任务'); await flush();
    runtime.adminTask.mockResolvedValue(response(stopped()));
    await store.forceStop('task-a');
    expect(runtime.adminStop).toHaveBeenCalledWith('task-a', { ifMatch: '"rev-7"', key: expect.any(String) });
    expect(runtime.memberStop).not.toHaveBeenCalled();
  });

  it('stops untracked historical other-owner tasks using their detail ETag', async () => {
    runtime.auth!.me.user.role = 'admin';
    const store = useTaskStore(); const done = vi.fn();
    const history = task({ user_id: 'other-owner', type: 'system.any-type', revision: '22' });
    runtime.adminTask.mockResolvedValue(response(stopped()));
    expect(store.tracked).toHaveLength(0);
    expect(await store.forceStop(history.id, { task: history, etag: '"history-detail-22"', onDone: done })).toBe(true);
    expect(runtime.adminStop).toHaveBeenCalledWith(history.id, { ifMatch: '"history-detail-22"', key: expect.any(String) });
    expect(store.tracked).toHaveLength(1); expect(done).toHaveBeenCalledTimes(1);
  });

  it('does not replace an uncertain historical request with a refreshed detail version', async () => {
    runtime.auth!.me.user.role = 'admin';
    const store = useTaskStore();
    runtime.adminStop.mockRejectedValueOnce(new ApiError(503, { code: 'service_unavailable', message: '', details: null }, 'uncertain', null));
    await store.forceStop('task-a', { task: task(), etag: '"original-detail"' }); await flush();
    const original = runtime.adminStop.mock.calls[0]; const done = vi.fn();
    runtime.adminTask.mockResolvedValue(response(stopped()));
    expect(await store.forceStop('task-a', { task: task({ revision: '99' }), etag: '"new-detail"', onDone: done })).toBe(true);
    expect(runtime.adminStop.mock.calls[1]).toEqual(original); expect(done).toHaveBeenCalledTimes(1);
  });

  it.each(['cancelled', 'failed', 'succeeded'] as const)('does not submit a force-stop for terminal %s history', async (status) => {
    runtime.memberTask.mockResolvedValue(response(task({ status })));
    const store = useTaskStore(); store.track('task-a', '已结束'); await flush();
    expect(await store.forceStop('task-a')).toBe(false);
    expect(runtime.memberStop).not.toHaveBeenCalled();
  });

  it('does not submit when the server explicitly denies force-stop capability', async () => {
    runtime.memberTask.mockResolvedValue(response(task({ can_force_stop: false, force_stop_blocked_reason: 'task_stop_not_allowed' })));
    const store = useTaskStore(); store.track('task-a', '任务'); await flush();
    expect(await store.forceStop('task-a')).toBe(false);
    expect(runtime.memberStop).not.toHaveBeenCalled();
  });

  it('retries an uncertain submission with the same key and original ETag after a fresh poll', async () => {
    const store = useTaskStore(); store.track('task-a', '任务'); await flush();
    runtime.memberStop.mockRejectedValueOnce(new ApiError(503, { code: 'service_unavailable', message: '', details: null }, 'request-error', null));
    runtime.memberTask.mockResolvedValue(response(task({ revision: '9' })));
    expect(await store.forceStop('task-a')).toBe(false); await flush();
    expect(store.tracked[0]!.stop.uncertain).toBe(true);
    const original = runtime.memberStop.mock.calls[0];
    runtime.memberTask.mockResolvedValue(response(stopped()));
    expect(await store.forceStop('task-a')).toBe(true);
    expect(runtime.memberStop.mock.calls[1]).toEqual(original);
  });

  it('shows a clear system-task permission failure and permits a fresh intentional retry', async () => {
    const store = useTaskStore(); store.track('task-a', '系统任务'); await flush();
    runtime.memberStop.mockRejectedValueOnce(new ApiError(403, { code: 'task_stop_not_allowed', message: '', details: null }, 'request-error', null));
    expect(await store.forceStop('task-a')).toBe(false);
    expect(store.tracked[0]!.stop.feedback?.text).toContain('系统执行任务仅管理员可强停');
    expect(store.tracked[0]!.stop.intent).toBeNull();
    expect(store.tracked[0]!.task?.status).toBe('running');
  });

  it('blocks duplicate submission and ignores an old poll arriving after force-stop', async () => {
    const store = useTaskStore(); store.track('task-a', '任务'); await flush();
    const oldPoll = Promise.withResolvers<unknown>();
    runtime.memberTask.mockReturnValueOnce(oldPoll.promise);
    await vi.advanceTimersByTimeAsync(2000);
    const stop = Promise.withResolvers<unknown>();
    runtime.memberStop.mockReturnValueOnce(stop.promise);
    const pending = store.forceStop('task-a');
    expect(await store.forceStop('task-a')).toBe(false);
    runtime.memberTask.mockResolvedValue(response(stopped()));
    stop.resolve(response(stopped())); await pending; await flush();
    oldPoll.resolve(response(task({ revision: '7' }))); await flush();
    expect(runtime.memberStop).toHaveBeenCalledTimes(1);
    expect(store.tracked[0]!.task?.status).toBe('cancelled');
  });

  it('does not apply a late response, notification or page callback after account reset', async () => {
    const done = vi.fn();
    const store = useTaskStore(); store.track('task-a', '任务', done); await flush();
    const stop = Promise.withResolvers<unknown>();
    runtime.memberStop.mockReturnValueOnce(stop.promise);
    const pending = store.forceStop('task-a');
    store.$reset(); runtime.auth!.me.user.id = 'user-b';
    stop.resolve(response(stopped())); expect(await pending).toBe(false);
    expect(store.tracked).toHaveLength(0);
    expect(done).not.toHaveBeenCalled();
    expect(runtime.success).not.toHaveBeenCalled();
  });
});
