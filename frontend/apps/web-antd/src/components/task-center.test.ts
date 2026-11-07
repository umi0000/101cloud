import type { App, Slots } from 'vue';
import type { Task } from '#/api/types';

import { createApp, h, nextTick } from 'vue';

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

const runtime = vi.hoisted(() => ({
  push: vi.fn(),
  tasks: { open: true, active: [], tracked: [] as Array<{ taskId: string; task: Task; label: string; requestId: string }>, clearFinished: vi.fn(), forceStop: vi.fn() },
}));
vi.mock('vue-router', () => ({ useRouter: () => ({ push: runtime.push }) }));
vi.mock('#/store/tasks', () => ({ useTaskStore: () => runtime.tasks }));
vi.mock('#/store/auth', () => ({ useAuthStore: () => ({ isAdmin: false }) }));
vi.mock('./error-feedback.vue', () => ({ default: () => null }));
vi.mock('./status-tag.vue', () => ({ default: () => null }));
vi.mock('./time-text.vue', () => ({ default: () => null }));
vi.mock('./confirm-action.vue', () => ({ default: {
  props: ['open', 'onConfirm', 'description', 'riskText'],
  emits: ['update:open'],
  setup: (props: { open: boolean; onConfirm: () => unknown; description: string; riskText: string }, { emit }: { emit: (name: string, value: boolean) => void }) => () => props.open ? h('div', { role: 'dialog' }, [
    h('p', props.description), h('p', props.riskText),
    h('button', { onClick: () => emit('update:open', false) }, '取消强停'),
    h('button', { onClick: props.onConfirm }, '确认强停'),
  ]) : null,
} }));
vi.mock('ant-design-vue', () => {
  const container = { setup: (_: unknown, { slots }: { slots: Slots }) => () => h('div', slots.default?.()) };
  return { Badge: container, Tooltip: container, Drawer: container, Button: 'button', Empty: () => null };
});

import TaskCenter from './task-center.vue';

let app: App;
let root: HTMLElement;
beforeEach(() => { runtime.push.mockReset(); runtime.tasks.forceStop.mockReset(); runtime.tasks.open = true; runtime.tasks.tracked = []; });
afterEach(() => { app?.unmount(); root?.remove(); });

describe('failed creation task center entry', () => {
  it.each(['failed', 'unknown'] as const)('retains the initialization error route after %s creation cleanup completes', async (status) => {
    const task: Task = {
      id: 'create-task', instance_id: 'hidden-instance', user_id: 'user-a', type: 'instance.create', status,
      created_at: '2026-11-04T00:00:00Z', started_at: null, finished_at: null,
      error_code: null, phase: 'verify', revision: '1', next_action: 'inspect_failure', node_id: null, steps: [],
      creation_cleanup: { status: 'completed', billing_status: 'refunded', instance_name: 'safe retired name' },
    };
    runtime.tasks.tracked = [{ taskId: task.id, task, label: '创建实例', requestId: '' }];
    root = document.createElement('div');
    document.body.append(root);
    app = createApp(TaskCenter);
    app.mount(root);
    await nextTick();
    expect(root.textContent).not.toContain('再决定是否重新提交');
    expect(root.textContent).not.toContain('退款状态');
    const entry = [...root.querySelectorAll('button')].find((button) => button.textContent === '查看初始化');
    expect(entry).toBeDefined();
    entry!.click();
    expect(runtime.push).toHaveBeenCalledWith({ name: 'MemberInstanceInitialization', params: { taskId: task.id } });
    expect(runtime.tasks.open).toBe(false);
    expect(task.status).toBe(status);
  });
});

describe('force-stop task center controls', () => {
  function mountTask(status: Task['status'], type = 'new.task.type', overrides: Partial<Task> = {}) {
    const task: Task = {
      id: 'task-a', instance_id: null, user_id: 'user-a', type, status,
      created_at: '2026-11-07T00:00:00Z', started_at: null, finished_at: null,
      error_code: null, phase: null, revision: '1', next_action: 'wait', node_id: null, steps: [],
      ...overrides,
    };
    runtime.tasks.tracked = [{ taskId: task.id, task, label: '任务', requestId: '' }];
    root = document.createElement('div'); document.body.append(root);
    app = createApp(TaskCenter); app.mount(root);
  }

  it.each(['queued', 'running', 'unknown', 'waiting'] as const)('offers confirmation for every nonterminal %s task without a type whitelist', async (status) => {
    mountTask(status);
    [...root.querySelectorAll('button')].find((button) => button.textContent === '强制停止')!.click();
    await nextTick();
    expect(root.querySelector('[role="dialog"]')?.textContent).toContain('不保证已经发出的外部操作停止');
    expect(root.textContent).toContain('不代表实例、IP、GPU 或计费预留已经释放');
    [...root.querySelectorAll('button')].find((button) => button.textContent === '取消强停')!.click();
    await nextTick();
    expect(runtime.tasks.forceStop).not.toHaveBeenCalled();
    expect(root.querySelector('[role="dialog"]')).toBeNull();
  });

  it('submits only after explicit confirmation and retains the task entry', async () => {
    mountTask('running', 'image.prepare');
    [...root.querySelectorAll('button')].find((button) => button.textContent === '强制停止')!.click();
    await nextTick();
    [...root.querySelectorAll('button')].find((button) => button.textContent === '确认强停')!.click();
    expect(runtime.tasks.forceStop).toHaveBeenCalledWith('task-a');
    expect(runtime.tasks.tracked).toHaveLength(1);
  });

  it('keeps a denied nonterminal action visible with a server-derived reason', () => {
    mountTask('running', 'new.task.type', { can_force_stop: false, force_stop_blocked_reason: 'task_stop_not_allowed' });
    const stop = [...root.querySelectorAll('button')].find((button) => button.textContent === '强制停止');
    expect(stop?.disabled).toBe(true);
    expect(root.textContent).toContain('系统执行任务仅管理员可强停');
  });

  it('preserves explicit external reconciliation and resource warnings on cancelled history', () => {
    mountTask('cancelled', 'image.prepare', { force_stop: { orchestration_stopped: true, external_state: 'requires_reconciliation', requires_reconciliation: true, resources_retained: true, reconciliation_completed: false } });
    expect(root.textContent).toContain('历史记录保留');
    expect(root.textContent).toContain('资源与计费不视为已释放');
    expect(root.textContent).toContain('勿重复提交原操作');
  });

  it('distinguishes completed external reconciliation without claiming resource release', () => {
    mountTask('cancelled', 'image.prepare', { force_stop: { orchestration_stopped: true, external_state: 'requires_reconciliation', requires_reconciliation: false, resources_retained: true, reconciliation_completed: true } });
    expect(root.textContent).toContain('外部结果核对已完成');
    expect(root.textContent).not.toContain('服务端确认尚未派发外部操作');
    expect(root.textContent).not.toContain('资源与计费已释放');
  });

  it.each(['cancelled', 'failed', 'succeeded'] as const)('does not offer force-stop for terminal %s tasks', (status) => {
    mountTask(status);
    expect([...root.querySelectorAll('button')].some((button) => button.textContent === '强制停止')).toBe(false);
  });
});
