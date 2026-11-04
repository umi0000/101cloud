import type { App, Slots } from 'vue';

import { createApp, h, nextTick, ref, shallowRef } from 'vue';

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { ApiError } from '#/api/request';
import Detail from './detail.vue';

const runtime = vi.hoisted(() => ({
  instance: {} as Record<string, any>, get: vi.fn(), remove: vi.fn(), replace: vi.fn(),
  poll: undefined as undefined | (() => Promise<void>),
  done: undefined as undefined | (() => void),
  confirmation: {} as Record<string, any>,
}));
vi.mock('vue-router', () => ({ useRoute: () => ({ params: { id: '42' } }), useRouter: () => ({ replace: runtime.replace }) }));
vi.mock('#/api/backend', () => ({
  instancesApi: { get: runtime.get, delete: runtime.remove, network: async () => ({ data: [] }) },
  opsApi: {}, catalogApi: {}, nodesApi: {},
}));
vi.mock('#/store/tasks', () => ({ useTaskStore: () => ({ track: vi.fn() }) }));
vi.mock('#/composables/api', () => ({
  errorToast: vi.fn(),
  useResource: (fetch: () => Promise<any> | null) => {
    const data = shallowRef<any>(null);
    return { data, etag: ref('revision'), requestId: ref(''), reload: async () => {
      const response = await fetch();
      if (response) data.value = response.data;
    } };
  },
  usePolling: (poll: () => Promise<void>) => { runtime.poll = poll; },
  useAction: () => ({ pending: ref(false), resetKey: vi.fn(), run: (fn: (key: string) => Promise<any>) => fn('key') }),
  trackAccepted: (_: unknown, __: string, done: () => void) => { runtime.done = done; },
}));
vi.mock('#/components/confirm-action.vue', () => ({ default: {
  props: ['open', 'confirmName', 'disabled', 'riskText', 'title', 'onConfirm', 'description'],
  setup: (props: Record<string, any>) => () => {
    if (!props.open) return null;
    runtime.confirmation = props;
    return h('section', { role: 'dialog' }, [h('button', { onClick: props.onConfirm, disabled: props.disabled }, props.title)]);
  },
} }));
vi.mock('@vben/common-ui', () => ({ Page: { setup: (_: unknown, { slots }: { slots: Slots }) => () => h('main', slots.default?.()) } }));
vi.mock('ant-design-vue', () => {
  const container = { setup: (_: unknown, { slots }: { slots: Slots }) => () => h('div', slots.default?.()) };
  return { ...Object.fromEntries(['Alert', 'Card', 'Descriptions', 'DescriptionsItem', 'Empty', 'Input', 'Modal', 'RadioButton', 'RadioGroup', 'Result', 'Space', 'Spin', 'TabPane', 'Table', 'Tabs', 'Tag', 'TypographyText'].map((key) => [key, container])), Button: 'button' };
});
vi.mock('#/components/error-feedback.vue', () => ({ default: () => null }));
vi.mock('#/components/status-tag.vue', () => ({ default: () => null }));
vi.mock('#/components/time-text.vue', () => ({ default: () => null }));
vi.mock('#/components/user-name.vue', () => ({ default: () => null }));
vi.mock('./components/console-drawer.vue', () => ({ default: () => null }));
vi.mock('./components/gpu-panel.vue', () => ({ default: () => null }));
vi.mock('./components/gpu-policy-panel.vue', () => ({ default: () => null }));
vi.mock('./components/grace-tag.vue', () => ({ default: () => null }));
vi.mock('./components/metrics-panel.vue', () => ({ default: () => null }));
vi.mock('./components/resize-dialog.vue', () => ({ default: () => null }));
vi.mock('./components/task-panel.vue', () => ({ default: () => null }));
vi.mock('./components/shared', () => ({ useNow: () => ref(Date.now()) }));


let app: App;
let root: HTMLElement;
async function flush() {
  for (let index = 0; index < 12; index++) await Promise.resolve();
  await nextTick();
}
async function mount() {
  root = document.createElement('div');
  document.body.append(root);
  app = createApp(Detail);
  app.component('RouterLink', { setup: (_: unknown, { slots }: { slots: Slots }) => () => h('a', slots.default?.()) });
  app.mount(root);
  await flush();
}
async function openDelete() {
  (root.querySelector('[data-testid="admin-instance-delete"]') as HTMLButtonElement).click();
  await flush();
}
beforeEach(() => {
  vi.clearAllMocks();
  runtime.done = undefined;
  runtime.instance = {
    id: '42', name: 'exact-name', provisioning_state: 'ready', state_stale: false,
    observed_state: 'stopped', desired_state: 'stopped', active_task_id: null,
    capabilities: { delete: true, start: true, stop: true, resize: true },
    resources: { vcpus: 1, memory_bytes: 1024, disk_bytes: 1024 },
  };
  runtime.get.mockImplementation(async () => ({ data: { ...runtime.instance } }));
  runtime.remove.mockResolvedValue({ data: { task_id: 'delete-task' } });
});
afterEach(() => { app?.unmount(); root?.remove(); });

describe('admin instance cleanup deletion', () => {
  it.each([
    { active_task_id: 'old-task' },
    { state_stale: true, observed_state: 'unknown' },
    { provisioning_state: 'pending', capabilities: { delete: false } },
  ])('allows explicit cleanup when ordinary lifecycle deletion is unavailable: %o', async (state) => {
    Object.assign(runtime.instance, state);
    await mount();
    expect([...root.querySelectorAll('button')].filter((button) => ['启动', '停止', '重启', '修改实例规格'].includes(button.textContent ?? '')).every((button) => button.disabled)).toBe(true);
    await openDelete();
    expect(runtime.confirmation.confirmName).toBe('exact-name');
    expect(runtime.confirmation.riskText).toContain('取消已有操作');
    expect(runtime.confirmation.description).toContain('历史计费和审计记录保留');
    expect(runtime.confirmation.disabled).toBe(false);
    await runtime.confirmation.onConfirm();
    expect(runtime.remove).toHaveBeenCalledWith('42', {
      cleanup_stuck: true, mode: 'force', confirm_data_loss_risk: true, confirm_name: 'exact-name', delete_data: true,
    }, { ifMatch: 'revision', key: 'key' });
    expect(runtime.replace).not.toHaveBeenCalled();
    expect(runtime.done).toBeTypeOf('function');
  });

  it.each(['stopped', 'running'])('preserves ordinary ready %s deletion', async (state) => {
    runtime.instance.observed_state = state;
    await mount();
    await openDelete();
    await runtime.confirmation.onConfirm();
    const body = runtime.remove.mock.calls[0]?.[1];
    expect(body).not.toHaveProperty('cleanup_stuck');
    expect(body).toEqual(state === 'running'
      ? { mode: 'force', confirm_data_loss_risk: true, confirm_name: 'exact-name', delete_data: true }
      : { confirm_name: 'exact-name', delete_data: true });
  });

  it('keeps an open confirmation usable when polling discovers an active task', async () => {
    await mount();
    await openDelete();
    runtime.instance.active_task_id = 'old-task';
    await runtime.poll?.();
    await flush();
    expect(runtime.confirmation.title).toBe('强制清理删除实例');
    expect(runtime.confirmation.disabled).toBe(false);
    expect(runtime.confirmation.riskText).not.toBe('');
    await runtime.confirmation.onConfirm();
    expect(runtime.remove.mock.calls[0]?.[1]?.cleanup_stuck).toBe(true);
  });

  it('refreshes after task completion and navigates only once deletion is confirmed', async () => {
    await mount();
    await openDelete();
    await runtime.confirmation.onConfirm();
    expect(runtime.replace).not.toHaveBeenCalled();
    runtime.instance.desired_state = 'deleted';
    runtime.instance.active_task_id = null;
    runtime.done?.();
    await flush();
    expect(runtime.replace).toHaveBeenCalledWith('/admin/instances');
  });

  it('returns to the list on confirmed missing detail, not on offline errors', async () => {
    await mount();
    runtime.get.mockRejectedValueOnce(new Error('offline'));
    await expect(runtime.poll?.()).rejects.toThrow('offline');
    expect(runtime.replace).not.toHaveBeenCalled();
    runtime.get.mockRejectedValueOnce(new ApiError(404, { code: 'not_found', message: '', details: null }, '', null));
    await runtime.poll?.();
    expect(runtime.replace).toHaveBeenCalledWith('/admin/instances');
  });
});
