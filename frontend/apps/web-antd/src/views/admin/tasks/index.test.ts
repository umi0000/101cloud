import type { App, Slots } from 'vue';
import type { Task } from '#/api/types';
import type { TaskStopSource } from '#/store/tasks';

import { createApp, h, nextTick, reactive, ref } from 'vue';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

const runtime = vi.hoisted(() => ({
  task: vi.fn(), tasks: vi.fn(), forceStop: vi.fn(), errorToast: vi.fn(),
  auth: null as null | { me: { user: { id: string; role: string } } },
  tracked: [] as Array<{ taskId: string; task: Task; etag: string; stop: { pending: boolean; uncertain: boolean; feedback: null } }>,
}));
vi.mock('vue-router', () => ({ useRoute: () => ({ query: {} }) }));
vi.mock('#/api/backend', () => ({ opsApi: { task: runtime.task, tasks: runtime.tasks } }));
vi.mock('#/store/auth', () => ({ useAuthStore: () => runtime.auth }));
vi.mock('#/store/tasks', () => ({ useTaskStore: () => ({ tracked: runtime.tracked, forceStop: runtime.forceStop }) }));
vi.mock('#/components/error-feedback.vue', () => ({ default: () => null }));
vi.mock('#/components/status-tag.vue', () => ({ default: { props: ['value'], setup: (props: { value: string }) => () => h('span', props.value) } }));
vi.mock('#/components/time-text.vue', () => ({ default: () => null }));
vi.mock('#/components/user-name.vue', () => ({ default: () => null }));
vi.mock('#/components/cursor-pager.vue', () => ({ default: () => null }));
vi.mock('#/components/confirm-action.vue', () => ({ default: () => null }));
vi.mock('#/components/task-force-stop-confirm.vue', () => ({ default: {
  props: ['open', 'onConfirm', 'disabled', 'pending'],
  emits: ['update:open'],
  setup: (props: { open: boolean; onConfirm: () => unknown; disabled: boolean; pending: boolean }, { emit }: { emit: (event: string, value: boolean) => void }) => () => props.open ? h('div', { role: 'dialog' }, [
    h('button', { onClick: () => emit('update:open', false) }, '取消强停'),
    h('button', { disabled: props.disabled || props.pending, onClick: props.onConfirm }, '确认强停'),
  ]) : null,
} }));
vi.mock('@vben/common-ui', () => ({ Page: { setup: (_: unknown, { slots }: { slots: Slots }) => () => h('main', slots.default?.()) } }));
vi.mock('ant-design-vue', () => {
  const container = { setup: (_: unknown, { slots }: { slots: Slots }) => () => h('div', [slots.extra?.(), slots.default?.()]) };
  return {
    Button: 'button', Card: container, Descriptions: container, DescriptionsItem: container, Input: 'input', Select: 'select',
    Drawer: { props: ['open'], setup: (props: { open: boolean }, { slots }: { slots: Slots }) => () => props.open ? h('aside', [slots.extra?.(), slots.default?.()]) : null },
    Table: { props: ['columns', 'dataSource', 'customRow'], setup: (props: { columns: Array<{ key?: string; dataIndex?: string }>; dataSource: Task[]; customRow?: (task: Task) => { onClick: () => unknown } }, { slots }: { slots: Slots }) => () => h('section', props.dataSource.map((record) => h('div', { 'data-task-id': record.id, onClick: props.customRow?.(record).onClick }, props.columns.map((column) => h('span', slots.bodyCell?.({ column, record })))))) },
  };
});
vi.mock('#/composables/api', () => {
  return {
    errorToast: runtime.errorToast, trackAccepted: vi.fn(), useAction: () => ({ run: vi.fn() }),
    useCursorList: () => {
      const items = ref<Task[]>([]);
      return { items, loading: ref(false), requestId: ref('list-request'),
        load: async () => { items.value = (await runtime.tasks()).data.items; },
        hasNext: () => false, hasPrev: () => false, page: () => 1, nextPage: vi.fn(), prevPage: vi.fn(),
      };
    },
  };
});

import AdminTasks from './index.vue';

function task(overrides: Partial<Task> = {}): Task {
  return { id: 'history-other-owner', user_id: 'other-owner', instance_id: null, node_id: null, type: 'arbitrary.task', status: 'running', revision: '7', phase: null, error_code: null, next_action: 'wait', steps: [], created_at: '2026-11-07T00:00:00Z', started_at: null, finished_at: null, can_force_stop: true, ...overrides };
}
let app: App;
let root: HTMLElement;
async function flush() { for (let i = 0; i < 10; i++) { await Promise.resolve(); await nextTick(); } }
async function mount() {
  root = document.createElement('div'); document.body.append(root);
  app = createApp(AdminTasks); app.component('RouterLink', { setup: (_: unknown, { slots }: { slots: Slots }) => () => h('a', slots.default?.()) }); app.mount(root); await flush();
}
function button(text: string, scope: ParentNode = root) { return [...scope.querySelectorAll('button')].find((element) => element.textContent === text)!; }
beforeEach(() => {
  vi.resetAllMocks(); runtime.auth = reactive({ me: { user: { id: 'admin-a', role: 'admin' } } }); runtime.tracked = reactive([]);
  runtime.tasks.mockResolvedValue({ data: { items: [task()], next_cursor: null } });
  runtime.task.mockResolvedValue({ data: task(), etag: '"detail-etag-7"', requestId: 'detail-request' });
  runtime.forceStop.mockImplementation(async (_id: string, source: TaskStopSource) => {
    runtime.tracked.push({ taskId: source.task.id, task: task({ status: 'cancelled', revision: '8', force_stop: { orchestration_stopped: true, external_state: 'requires_reconciliation', requires_reconciliation: true } }), etag: '"rev-8"', stop: { pending: false, uncertain: false, feedback: null } });
    source.onDone?.(); return true;
  });
});
afterEach(() => { app?.unmount(); root?.remove(); });

describe('administrator historical task force-stop', () => {
  it('loads a nontracked other-owner task and permits cancellation without submitting', async () => {
    await mount(); expect(runtime.tracked).toHaveLength(0);
    button('强制停止').click(); await flush();
    expect(runtime.task).toHaveBeenCalledWith('history-other-owner');
    expect(root.querySelector('[role="dialog"]')).not.toBeNull();
    button('取消强停').click(); await flush();
    expect(runtime.forceStop).not.toHaveBeenCalled(); expect(runtime.tracked).toHaveLength(0);
  });

  it('submits the detail ETag and refreshes list and detail after success', async () => {
    await mount(); button('强制停止').click(); await flush();
    const ended = task({ status: 'cancelled', revision: '8', force_stop: { orchestration_stopped: true, external_state: 'requires_reconciliation', requires_reconciliation: true } });
    runtime.task.mockResolvedValue({ data: ended, etag: '"rev-8"', requestId: 'updated' });
    runtime.tasks.mockResolvedValue({ data: { items: [ended] } });
    button('确认强停').click(); await flush();
    expect(runtime.forceStop).toHaveBeenCalledWith('history-other-owner', expect.objectContaining({ task: expect.objectContaining({ user_id: 'other-owner' }), etag: '"detail-etag-7"' }));
    expect(runtime.tasks).toHaveBeenCalledTimes(2); expect(runtime.task).toHaveBeenCalledTimes(2);
    expect(root.querySelector('aside')?.textContent).toContain('资源与计费不视为已释放');
    expect(root.querySelector('aside')?.textContent).toContain('请求核对');
    expect([...root.querySelectorAll('aside button')].some((element) => element.textContent === '强制停止')).toBe(false);
  });

  it('disables capability-denied actions without guessing the task type', async () => {
    runtime.tasks.mockResolvedValue({ data: { items: [task({ can_force_stop: false, force_stop_blocked_reason: 'task_stop_not_allowed' })] } });
    await mount(); expect(button('强制停止').disabled).toBe(true); expect(root.textContent).toContain('系统执行任务仅管理员可强停');
  });

  it('does not let an old detail response replace a newly selected task', async () => {
    const old = Promise.withResolvers<unknown>();
    runtime.tasks.mockResolvedValue({ data: { items: [task(), task({ id: 'new-task' })] } });
    runtime.task.mockReturnValueOnce(old.promise).mockResolvedValue({ data: task({ id: 'new-task' }), etag: '"new"', requestId: 'new' });
    await mount(); root.querySelector<HTMLElement>('[data-task-id="history-other-owner"]')!.click(); await flush();
    root.querySelector<HTMLElement>('[data-task-id="new-task"]')!.click(); await flush(); old.resolve({ data: task(), etag: '"old"', requestId: 'old' }); await flush();
    button('强制停止', root.querySelector('aside')!).click(); await flush(); button('确认强停').click(); await flush();
    expect(runtime.forceStop).toHaveBeenCalledWith('new-task', expect.objectContaining({ etag: '"new"' }));
  });
});
