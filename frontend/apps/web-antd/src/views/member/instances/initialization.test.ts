import type { App, Slots } from 'vue';
import type { Task } from '#/api/types';

import { createApp, h, nextTick, reactive } from 'vue';

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { ApiError } from '#/api/request';
import { CREATION_STAGES } from '#/utils/creation-progress';

const runtime = vi.hoisted(() => ({
  access: { loginExpired: false }, auth: { me: { user: { id: 'user-a', role: 'user' } } },
  route: { params: { taskId: 'task-user-a' } },
  task: vi.fn(), instance: vi.fn(), push: vi.fn(), poll: null as null | (() => unknown),
}));
vi.mock('@vben/stores', () => ({ useAccessStore: () => runtime.access }));
vi.mock('#/store/auth', () => ({ useAuthStore: () => runtime.auth }));
vi.mock('vue-router', () => ({ useRoute: () => runtime.route, useRouter: () => ({ push: runtime.push }) }));
vi.mock('#/api/backend', () => ({ memberApi: { task: runtime.task, instance: runtime.instance } }));
vi.mock('#/composables/api', () => ({ usePolling: (fn: () => unknown) => { runtime.poll = fn; } }));
vi.mock('#/components/os-icon.vue', () => ({ default: () => null }));
vi.mock('#/components/error-feedback.vue', () => ({ default: {
  props: ['feedback'],
  setup: (props: { feedback?: { text: string; details: string[]; stage: string } | null }) => () => props.feedback
    ? h('div', { 'data-error-feedback': '' }, [props.feedback.text, props.feedback.stage, ...props.feedback.details].join(' ')) : null,
} }));
vi.mock('ant-design-vue', () => ({ Button: 'button', Modal: {
  props: ['open'],
  setup: (props: { open: boolean }, { slots }: { slots: Slots }) => () => props.open
    ? h('section', { role: 'dialog' }, slots.default?.()) : null,
} }));

import Initialization from './initialization.vue';

const time = '2026-11-03T00:00:00Z';
function task(userId = 'user-a'): Task {
  return {
    id: `task-${userId}`, instance_id: `instance-${userId}`, user_id: userId,
    type: 'instance.create', status: 'succeeded', created_at: time, started_at: time, finished_at: time,
    error_code: null, next_action: '', node_id: 'node', phase: 'verify', revision: '1', steps: [],
    creation_progress: { initialization_pending: false, stages: CREATION_STAGES.map(({ key }) => ({
      key, status: key === 'gpu' || key === 'script' ? 'skipped' : 'succeeded',
    })) },
  };
}
function instance(userId = 'user-a') {
  return { id: `instance-${userId}`, user_id: userId, creation_task_id: `task-${userId}`,
    name: `name-${userId}`, image_os_name: 'Ubuntu', image_os_release: '24.04', login_mode: 'ssh_key',
    resources: { vcpus: 2, memory_bytes: '1073741824', disk_bytes: '10737418240' } };
}
let app: App;
let root: HTMLElement;
async function flush() {
  for (let i = 0; i < 8; ++i) await Promise.resolve();
  await nextTick();
}
function mount() {
  root = document.createElement('div');
  document.body.append(root);
  app = createApp(Initialization);
  app.mount(root);
}
beforeEach(() => {
  vi.useFakeTimers();
  runtime.task.mockReset();
  runtime.instance.mockReset();
  runtime.push.mockReset();
  runtime.auth = reactive({ me: { user: { id: 'user-a', role: 'user' } } });
  runtime.access = reactive({ loginExpired: false });
  runtime.route = reactive({ params: { taskId: 'task-user-a' } });
  runtime.task.mockResolvedValue({ data: task() });
  runtime.instance.mockResolvedValue({ data: instance() });
});
afterEach(() => {
  app?.unmount();
  root?.remove();
  vi.useRealTimers();
});

describe('initialization route recovery and isolation', () => {
  it('restores directly from task URL and enters only on a manual action', async () => {
    mount();
    await flush();
    expect(runtime.task).toHaveBeenCalledWith('task-user-a');
    expect(runtime.instance).toHaveBeenCalledWith('instance-user-a');
    expect(root.textContent).toContain('实例初始化完成');
    expect(root.textContent).toContain('name-user-a');
    expect(root.querySelector('[data-stage="gpu"]')).toBeNull();
    expect(root.querySelector('[data-stage="script"]')?.textContent).toContain('不适用');
    expect(root.querySelector('[role="dialog"]')).toBeNull();
    expect([...root.querySelectorAll('button')].some((button) => button.textContent === '查看失败信息')).toBe(false);
    expect(runtime.push).not.toHaveBeenCalled();
    const enter = [...root.querySelectorAll('button')].find((button) => button.textContent === '进入实例');
    enter!.click();
    expect(runtime.push).toHaveBeenCalledWith({ path: '/instances', query: { instance: 'instance-user-a' } });
  });

  it('discards a previous account response and clears its data synchronously', async () => {
    const { promise, resolve: resolveOld } = Promise.withResolvers<{ data: Task }>();
    runtime.task.mockImplementationOnce(() => promise);
    mount();
    runtime.task.mockResolvedValue({ data: task('user-b') });
    runtime.instance.mockResolvedValue({ data: instance('user-b') });
    runtime.auth.me.user.id = 'user-b';
    runtime.route.params.taskId = 'task-user-b';
    await flush();
    resolveOld({ data: task() });
    await flush();
    expect(root.textContent).not.toContain('name-user-a');
    expect(root.textContent).toContain('name-user-b');
    expect(runtime.instance).not.toHaveBeenCalledWith('instance-user-a');
  });

  it('preserves the last confirmed stages without activity or entry after a query interruption', async () => {
    const running = task();
    running.status = 'running';
    running.finished_at = null;
    running.creation_progress!.stages[1]!.status = 'running';
    runtime.task.mockResolvedValueOnce({ data: running });
    mount();
    await flush();
    runtime.task.mockRejectedValueOnce(new Error('offline'));
    await runtime.poll!();
    await flush();
    expect(root.textContent).toContain('状态读取中断');
    expect(root.querySelector('[data-stage="native"]')?.getAttribute('data-status')).toBe('running');
    expect(root.querySelector('.activity-line.is-active')).toBeNull();
    expect([...root.querySelectorAll('button')].some((button) => button.textContent === '进入实例')).toBe(false);
  });
  it.each(['failed', 'unknown'] as const)('reveals diagnostics only on request for %s without enabling instance entry', async (status) => {
    const failed = task();
    failed.status = status;
    failed.creation_progress!.stages[4]!.status = 'failed';
    failed.steps = [{ name: 'login.cloud-ready', status: 'failed', error_code: 'initialization_tool_missing',
      started_at: time, finished_at: time,
      error: { code: 'initialization_tool_missing', message: '', details: {
        stage: 'login.cloud-ready', reason: 'required_tool_missing', exit_code: 2, cloud_init_status: 'degraded',
      } },
    }];
    failed.error_code = failed.steps[0]!.error_code;
    failed.error = failed.steps[0]!.error;
    runtime.task.mockResolvedValueOnce({ data: failed });
    mount();
    await flush();
    expect(root.querySelector('h1')?.textContent).toBe('初始化失败');
    expect(root.querySelector('[data-stage="cloud_init"]')?.getAttribute('data-status')).toBe('failed');
    expect(root.querySelector('[role="dialog"]')).toBeNull();
    expect(root.textContent).not.toContain('原因：缺少初始化必需工具');
    const details = [...root.querySelectorAll('button')].find((button) => button.textContent === '查看失败信息');
    details!.click();
    await flush();
    const dialog = root.querySelector('[role="dialog"]')!;
    expect(dialog.textContent).toContain('原因：缺少初始化必需工具');
    expect(dialog.textContent).toContain('退出码：2');
    expect(dialog.textContent).toContain(status === 'unknown' ? '请勿重复创建' : '不会自动重新创建');
    expect(dialog.querySelectorAll('[data-error-feedback]')).toHaveLength(1);
    expect([...root.querySelectorAll('button')].some((button) => button.textContent === '进入实例')).toBe(false);
    [...dialog.querySelectorAll('button')].find((button) => button.textContent === '关闭')!.click();
    await flush();
    expect(root.querySelector('[role="dialog"]')).toBeNull();
  });
  it('preserves distinct diagnostic stages while suppressing the repeated task error', async () => {
    const failed = task();
    failed.status = 'failed';
    failed.error_code = 'initialization_tool_missing';
    failed.error = { code: 'initialization_tool_missing', message: '', details: {
      stage: 'login.cloud-ready', reason: 'required_tool_missing',
    } };
    failed.steps = ['login.cloud-ready', 'init.cloud-ready'].map((stage) => ({
      name: stage, status: 'failed', error_code: failed.error_code, started_at: time, finished_at: time,
      error: { code: 'initialization_tool_missing', message: '', details: { stage, reason: 'required_tool_missing' } },
    }));
    runtime.task.mockResolvedValueOnce({ data: failed });
    mount();
    await flush();
    [...root.querySelectorAll('button')].find((button) => button.textContent === '查看失败信息')!.click();
    await flush();
    const feedback = [...root.querySelectorAll('[data-error-feedback]')];
    expect(feedback).toHaveLength(2);
    expect(feedback.some((entry) => entry.textContent?.includes('登录配置初始化检查'))).toBe(true);
    expect(feedback.some((entry) => entry.textContent?.includes('实例初始化完成检查'))).toBe(true);
  });
  it.each(['failed', 'unknown'] as const)('restores a hidden %s creation using the safe task name without fetching its instance', async (status) => {
    const failed = task();
    failed.status = status;
    failed.creation_progress!.stages[4]!.status = 'failed';
    failed.creation_cleanup = { status: 'cleaning', billing_status: 'refunded', instance_name: 'safe retired name' };
    failed.error_code = 'initialization_tool_missing';
    failed.error = { code: 'initialization_tool_missing', message: '', details: {
      stage: 'login.cloud-ready', reason: 'required_tool_missing', exit_code: 2,
    } };
    runtime.task.mockResolvedValue({ data: failed });
    runtime.instance.mockRejectedValue(new ApiError(404, { code: 'not_found', message: '', details: null }, '', null));
    mount();
    await flush();
    expect(runtime.instance).not.toHaveBeenCalled();
    expect(root.querySelector('h1')?.textContent).toBe('初始化失败');
    expect(root.textContent).toContain('safe retired name');
    expect(root.textContent).not.toContain('实例摘要读取失败');
    expect(root.textContent).not.toContain('资源清理');
    expect(root.textContent).not.toContain('退款状态');
    expect(root.textContent).not.toContain('未成功交付不收费');
    expect([...root.querySelectorAll('button')].some((button) => button.textContent === '进入实例')).toBe(false);
    [...root.querySelectorAll('button')].find((button) => button.textContent === '查看失败信息')!.click();
    await flush();
    const dialog = root.querySelector('[role="dialog"]')!;
    expect(dialog.textContent).toContain('资源清理：正在清理');
    expect(dialog.textContent).toContain('退款状态：已完成冲正、清欠与预留释放');
    expect(dialog.textContent).toContain('未成功交付不收费，清理等待期间也不计费');
    expect(dialog.textContent).toContain('退出码：2');
    if (status === 'unknown') expect(dialog.textContent).toContain('执行结果仍需管理员核对');
    await runtime.poll!();
    await flush();
    expect(runtime.task).toHaveBeenCalledTimes(2);
    expect(runtime.instance).not.toHaveBeenCalled();
    expect(runtime.push).not.toHaveBeenCalled();
    expect(failed.status).toBe(status);
  });

  it('polls cleanup and billing to completion without replacing the original failed phase or diagnostics', async () => {
    const failed = task();
    failed.status = 'failed';
    failed.creation_progress!.stages[4]!.status = 'failed';
    failed.creation_cleanup = { status: 'pending', billing_status: 'pending', instance_name: 'safe retired name' };
    runtime.task.mockResolvedValueOnce({ data: failed });
    mount();
    await flush();
    [...root.querySelectorAll('button')].find((button) => button.textContent === '查看失败信息')!.click();
    await flush();
    expect(root.querySelector('[role="dialog"]')?.textContent).toContain('退款状态：等待处理');
    const reclaimed: Task = { ...failed, creation_cleanup: { ...failed.creation_cleanup, status: 'completed' } };
    runtime.task.mockResolvedValueOnce({ data: reclaimed });
    await runtime.poll!();
    await flush();
    expect(root.querySelector('[role="dialog"]')?.textContent).toContain('资源清理：已完成清理');
    runtime.task.mockResolvedValueOnce({ data: { ...reclaimed, creation_cleanup: { ...reclaimed.creation_cleanup!, billing_status: 'refunded' } } });
    await runtime.poll!();
    await flush();
    expect(root.querySelector('[role="dialog"]')?.textContent).toContain('退款状态：已完成冲正、清欠与预留释放');
    expect(root.querySelector('[data-stage="cloud_init"]')?.getAttribute('data-status')).toBe('failed');
    await runtime.poll!();
    expect(runtime.task).toHaveBeenCalledTimes(3);
    expect(runtime.instance).not.toHaveBeenCalled();
  });

  it('clears a previous summary error once the task reports expected creation cleanup', async () => {
    const failed = task();
    failed.status = 'failed';
    runtime.task.mockResolvedValueOnce({ data: failed });
    runtime.instance.mockRejectedValueOnce(new ApiError(404, { code: 'not_found', message: '', details: null }, '', null));
    mount();
    await flush();
    expect(root.textContent).toContain('实例摘要读取失败');
    runtime.task.mockResolvedValueOnce({ data: { ...failed, creation_cleanup: {
      status: 'unknown', billing_status: 'refunded', instance_name: 'safe retired name',
    } } });
    await runtime.poll!();
    await flush();
    expect(root.textContent).not.toContain('实例摘要读取失败');
    expect(root.textContent).toContain('safe retired name');
    expect(runtime.instance).toHaveBeenCalledTimes(1);
  });

  it('removes the last instance summary as soon as a later task response marks failed cleanup', async () => {
    const running = task();
    running.status = 'running';
    runtime.task.mockResolvedValueOnce({ data: running });
    mount();
    await flush();
    expect(root.textContent).toContain('name-user-a');
    runtime.task.mockResolvedValueOnce({ data: { ...running, status: 'failed', creation_cleanup: {
      status: 'cleaning', billing_status: 'refunded', instance_name: 'safe retired name',
    } } });
    await runtime.poll!();
    await flush();
    expect(root.textContent).not.toContain('name-user-a');
    expect(root.textContent).toContain('safe retired name');
    expect(runtime.instance).toHaveBeenCalledTimes(1);
  });
  it('closes failure details on account change and does not reopen them for another task', async () => {
    const failed = task();
    failed.status = 'unknown';
    failed.creation_cleanup = { status: 'unknown', billing_status: 'refunded', instance_name: 'safe retired name' };
    runtime.task.mockResolvedValueOnce({ data: failed });
    mount();
    await flush();
    [...root.querySelectorAll('button')].find((button) => button.textContent === '查看失败信息')!.click();
    await flush();
    expect(root.querySelector('[role="dialog"]')).not.toBeNull();
    runtime.task.mockResolvedValue({ data: task('user-b') });
    runtime.instance.mockResolvedValue({ data: instance('user-b') });
    runtime.auth.me.user.id = 'user-b';
    runtime.route.params.taskId = 'task-user-b';
    await flush();
    expect(root.querySelector('[role="dialog"]')).toBeNull();
    expect(root.textContent).not.toContain('name-user-a');
    expect(root.textContent).not.toContain('safe retired name');
  });
});
