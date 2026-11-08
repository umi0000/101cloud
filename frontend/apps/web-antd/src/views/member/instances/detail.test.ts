import type { App, Slots } from 'vue';
import type { Instance } from '#/api/types';
import { createApp, h, nextTick, reactive } from 'vue';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiError } from '#/api/request';

const runtime = vi.hoisted(() => ({
  auth: { me: { user: { id: '2', role: 'user' } } },
  access: { loginExpired: false },
  route: { params: { id: '10' }, query: {} as Record<string, string> },
  instance: vi.fn(),
  network: vi.fn(),
  gpuAllocations: vi.fn(),
  gpuPolicy: vi.fn(),
  power: vi.fn(),
  track: vi.fn(),
  delete: vi.fn(),
  gpuMount: vi.fn(),
  resizeMount: vi.fn(),
  replace: vi.fn(),
  push: vi.fn(),
}));
vi.mock('@vben/stores', () => ({ useAccessStore: () => runtime.access }));
vi.mock('#/store/auth', () => ({ useAuthStore: () => runtime.auth }));
vi.mock('#/store/tasks', () => ({
  useTaskStore: () => ({ tracked: [], track: runtime.track }),
}));
vi.mock('vue-router', () => ({
  onBeforeRouteLeave: vi.fn(),
  useRoute: () => runtime.route,
  useRouter: () => ({ replace: runtime.replace, push: runtime.push }),
}));
vi.mock('#/api/backend', () => ({
  memberConsoleApi: {},
  memberApi: {
    instance: runtime.instance,
    network: runtime.network,
    gpuAllocations: runtime.gpuAllocations,
    gpuPolicy: runtime.gpuPolicy,
    power: runtime.power,
    delete: runtime.delete,
  },
}));
vi.mock('#/composables/api', () => ({ usePolling: () => undefined }));
vi.mock('@vben/common-ui', () => ({
  Page: {
    setup:
      (_: unknown, { slots }: { slots: Slots }) =>
      () =>
        h('main', slots.default?.()),
  },
}));
vi.mock('ant-design-vue', () => {
  const container = {
    setup:
      (_: unknown, { slots }: { slots: Slots }) =>
      () =>
        h('div', [slots.default?.(), slots.action?.(), slots.overlay?.()]),
  };
  return {
    ...Object.fromEntries(
      [
        'Alert',
        'Card',
        'Dropdown',
        'Menu',
        'MenuItem',
        'TabPane',
        'Tabs',
        'TypographyText',
      ].map((k) => [k, container]),
    ),
    Button: 'button',
    Spin: () => null,
    Tag: 'span',
    Input: {
      props: ['value'],
      emits: ['update:value'],
      setup: (p: { value: string }, { emit }: { emit: (event: string, value: string) => void }) =>
        () => h('input', { value: p.value, onInput: (event: Event) => emit('update:value', (event.target as HTMLInputElement).value) }),
    },
    Checkbox: {
      props: ['checked'],
      emits: ['update:checked'],
      setup: (p: { checked: boolean }, { emit, slots }: { emit: (event: string, value: boolean) => void; slots: Slots }) =>
        () => h('label', [h('input', { type: 'checkbox', checked: p.checked, onChange: (event: Event) => emit('update:checked', (event.target as HTMLInputElement).checked) }), slots.default?.()]),
    },
    Modal: {
      props: ['open', 'title', 'okButtonProps'],
      emits: ['ok', 'update:open'],
      setup: (p: { open: boolean; title: string; okButtonProps?: { disabled: boolean } }, { emit, slots }: { emit: (event: string, value?: boolean) => void; slots: Slots }) =>
        () => p.open ? h('section', { 'data-dialog': p.title }, [
          h('h2', p.title),
          slots.default?.(),
          h('button', { 'data-confirm': p.title, disabled: p.okButtonProps?.disabled, onClick: () => emit('ok') }, '确认'),
          h('button', { onClick: () => emit('update:open', false) }, '取消'),
        ]) : null,
    },
    Alert: {
      props: ['message'],
      setup:
        (p: { message: string }, { slots }: { slots: Slots }) =>
        () =>
          h('div', [p.message, slots.action?.()]),
    },
    Empty: {
      props: ['description'],
      setup: (p: { description: string }) => () =>
        h('p', { 'data-empty': '' }, p.description),
    },
  };
});
vi.mock('#/components/error-feedback.vue', () => ({
  default: {
    props: ['feedback'],
    setup: () => () => h('span', { role: 'alert' }, 'error'),
  },
}));
vi.mock('#/components/os-icon.vue', () => ({ default: () => null }));
vi.mock('#/components/status-tag.vue', () => ({
  default: {
    props: ['label'],
    setup: (p: { label?: { text: string } }) => () => h('span', p.label?.text),
  },
}));
vi.mock('#/components/time-text.vue', () => ({ default: () => null }));
vi.mock('#/components/instances/console-drawer.vue', () => ({
  default: () => null,
}));
vi.mock('#/components/instances/resize-dialog.vue', () => ({
  default: {
    props: ['disabled'],
    emits: ['unresolved', 'submitting'],
    setup: (_: unknown, { emit }: { emit: (event: string, value: boolean) => void }) => {
      runtime.resizeMount();
      return () => h('div', [
        h('button', { onClick: () => emit('unresolved', true) }, 'resize unknown'),
        h('button', { onClick: () => emit('submitting', true) }, 'resize submitting'),
        h('button', { onClick: () => emit('submitting', false) }, 'resize settled'),
      ]);
    },
  },
}));
vi.mock('./components/gpu-config-panel.vue', () => ({
  default: {
    props: ['disabled'],
    emits: ['unresolved', 'submitting'],
    setup: (_: unknown, { emit }: { emit: (event: string, value: boolean) => void }) => {
      runtime.gpuMount();
      return () => h('div', [
        h('button', { onClick: () => emit('unresolved', true) }, 'gpu unknown'),
        h('button', { onClick: () => emit('submitting', true) }, 'gpu submitting'),
        h('button', { onClick: () => emit('submitting', false) }, 'gpu settled'),
      ]);
    },
  },
}));
vi.mock('./components/metrics-panel.vue', () => ({ default: () => null }));
vi.mock('./components/task-panel.vue', () => ({ default: () => null }));
import Detail from './detail.vue';
let app: App;
let root: HTMLElement;
async function flush() {
  for (let n = 0; n < 20; n++) await Promise.resolve();
  await nextTick();
}
function sample(id = '10', owner = '2'): Instance {
  return {
    id,
    user_id: owner,
    name: `private-${id}-${owner}`,
    instance_type: 'container',
    product_type: 'elastic',
    observed_state: 'running',
    desired_state: 'running',
    provisioning_state: 'ready',
    state_stale: false,
    active_task_id: null,
    resources: {
      vcpus: 4,
      memory_bytes: '8589934592',
      disk_bytes: '42949672960',
    },
    credit_enforcement: { status: 'normal' },
    capabilities: {
      console: true,
      start: false,
      stop: true,
      resize: true,
      delete: false,
      force_delete: true,
    },
  } as Instance;
}
function mount() {
  root = document.createElement('div');
  document.body.append(root);
  app = createApp(Detail);
  app.mount(root);
}
const button = (label: string) =>
  [...root.querySelectorAll<HTMLButtonElement>('button')].find(
    (b) => b.textContent === label,
  )!;
const cleanupDialog = () =>
  root.querySelector<HTMLElement>('[data-dialog="强制清理删除实例"]');
async function confirmCleanup(name = 'private-10-2') {
  const dialog = cleanupDialog()!;
  const input = dialog.querySelector<HTMLInputElement>('input:not([type="checkbox"])')!;
  input.value = name;
  input.dispatchEvent(new Event('input', { bubbles: true }));
  const risk = dialog.querySelector<HTMLInputElement>('input[type="checkbox"]')!;
  risk.checked = true;
  risk.dispatchEvent(new Event('change', { bubbles: true }));
  await nextTick();
  dialog.querySelector<HTMLButtonElement>('[data-confirm]')!.click();
  await flush();
}
beforeEach(() => {
  vi.clearAllMocks();
  runtime.route = reactive({ params: { id: '10' }, query: {} });
  runtime.auth = reactive({ me: { user: { id: '2', role: 'user' } } });
  runtime.access = reactive({ loginExpired: false });
  runtime.instance.mockResolvedValue({ data: sample(), etag: '"1"' });
  runtime.network.mockResolvedValue({ data: [] });
  runtime.gpuAllocations.mockResolvedValue({ data: [] });
  runtime.gpuPolicy.mockResolvedValue({
    data: { allowed_user_actions: ['stop', 'restart'] },
  });
  runtime.delete.mockResolvedValue({
    data: { task_id: '91', instance_id: '10', status: 'queued' },
  });
});
afterEach(() => {
  app?.unmount();
  root?.remove();
});

describe('member instance detail transitions', () => {
  it.each(['cancelled', 'unknown', 'running', undefined] as const)(
    'keeps %s task safety controls locked and restores normal permissions only after the active ID clears',
    async (status) => {
      runtime.instance.mockResolvedValue({
        data: { ...sample(), active_task_id: '90', active_task_status: status },
        etag: '"1"',
      });
      mount();
      await flush();
      expect(button('停止').disabled).toBe(true);
      expect(button('查看任务')).toBeDefined();
      if (status === 'cancelled') {
        expect(root.textContent).toContain('编排已停，待核对');
        expect(root.textContent).toContain('需管理员核对外部状态');
        expect(root.textContent).not.toMatch(/操作中|正在处理/);
      } else if (status === 'running') {
        expect(root.textContent).toContain('实例操作正在处理');
      } else {
        expect(root.textContent).toContain('操作结果待核对');
        expect(root.textContent).not.toContain('正在处理');
      }
      runtime.instance.mockResolvedValue({
        data: { ...sample(), active_task_status: null },
        etag: '"2"',
      });
      button('刷新').click();
      await flush();
      expect(button('停止').disabled).toBe(false);
      expect(root.textContent).not.toContain('编排已停，待核对');
      expect(runtime.power).not.toHaveBeenCalled();
    },
  );
  it('never renders another owner response and clears old details before a route change', async () => {
    mount();
    await flush();
    expect(root.textContent).toContain('private-10-2');
    const pending = Promise.withResolvers<unknown>();
    runtime.instance.mockReturnValueOnce(pending.promise);
    runtime.route.params.id = '11';
    await nextTick();
    expect(root.textContent).not.toContain('private-10-2');
    pending.resolve({ data: sample('11', '3'), etag: '"2"' });
    await flush();
    expect(root.textContent).not.toContain('private-11-3');
    expect(root.querySelector('[data-empty]')).not.toBeNull();
  });
  it('discards an old account response after switching accounts', async () => {
    const old = Promise.withResolvers<unknown>();
    runtime.instance.mockReturnValueOnce(old.promise);
    mount();
    runtime.instance.mockResolvedValue({
      data: sample('10', '3'),
      etag: '"2"',
    });
    runtime.auth.me.user.id = '3';
    await flush();
    old.resolve({ data: sample(), etag: '"1"' });
    await flush();
    expect(root.textContent).toContain('private-10-3');
    expect(root.textContent).not.toContain('private-10-2');
  });
  it('removes a hidden or removed instance rather than retaining its previous configuration', async () => {
    mount();
    await flush();
    runtime.instance.mockRejectedValueOnce(
      new ApiError(
        404,
        { code: 'not_found', message: '', details: null },
        '',
        null,
      ),
    );
    button('刷新').click();
    await flush();
    expect(root.textContent).not.toContain('private-10-2');
    expect(root.querySelector('[data-empty]')).not.toBeNull();
  });
  it('does not expose zero GPU or empty networking before independent resources resolve', async () => {
    const gpu = Promise.withResolvers<unknown>();
    const net = Promise.withResolvers<unknown>();
    runtime.gpuAllocations.mockReturnValue(gpu.promise);
    runtime.network.mockReturnValue(net.promise);
    mount();
    await flush();
    expect(root.textContent).not.toContain('0 张');
    expect(root.querySelectorAll('[data-empty]')).toHaveLength(0);
    gpu.resolve({ data: [] });
    net.resolve({ data: [] });
    await flush();
    expect(root.textContent).toContain('0 张');
    expect(root.querySelectorAll('[data-empty]').length).toBeGreaterThan(0);
  });
  it('freezes the original request and ETag after an uncertain stop while disabling new actions', async () => {
    runtime.power.mockRejectedValueOnce(
      new ApiError(
        503,
        { code: 'unavailable', message: '', details: null },
        '',
        null,
      ),
    );
    mount();
    await flush();
    button('停止').click();
    await nextTick();
    root.querySelector<HTMLButtonElement>('[data-confirm="停止实例"]')!.click();
    await flush();
    const first = runtime.power.mock.calls[0];
    expect(first?.[2]).toEqual({ mode: 'graceful' });
    expect(button('停止').disabled).toBe(true);
    runtime.instance.mockResolvedValue({
      data: { ...sample(), active_task_id: '90' },
      etag: '"9"',
    });
    button('刷新').click();
    await flush();
    runtime.power.mockResolvedValueOnce({
      data: { task_id: '90', instance_id: '10', status: 'queued' },
    });
    button('重试原请求').click();
    await flush();
    expect(runtime.power.mock.calls[1]).toEqual(first);
    expect(runtime.track).toHaveBeenCalledWith('90', '停止实例');
    expect(button('强制清理删除').disabled).toBe(false);
  });
  it('disables lifecycle changes when the instance state is stale', async () => {
    runtime.instance.mockResolvedValue({
      data: { ...sample(), state_stale: true },
      etag: '"1"',
    });
    mount();
    await flush();
    expect(button('停止').disabled).toBe(true);
    expect(runtime.power).not.toHaveBeenCalled();
  });
  it.each(['running', 'unknown', 'cancelled'] as const)(
    'offers explicit cleanup during a %s backend barrier despite stale and unready state',
    async (status) => {
      runtime.instance.mockResolvedValue({
        data: {
          ...sample(),
          active_task_id: '90',
          active_task_status: status,
          provisioning_state: 'unknown',
          state_stale: true,
        },
        etag: '"1"',
      });
      mount();
      await flush();
      expect(button('停止').disabled).toBe(true);
      expect(button('强制清理删除').disabled).toBe(false);
      button('强制清理删除').click();
      await nextTick();
      expect(cleanupDialog()?.textContent).toContain('取消已有操作');
      expect(cleanupDialog()?.textContent).toContain('离线或请求失败不代表已删除');
      await confirmCleanup();
      expect(runtime.delete).toHaveBeenCalledWith('10', {
        cleanup_stuck: true,
        mode: 'force',
        confirm_data_loss_risk: true,
        confirm_name: 'private-10-2',
        delete_data: true,
      }, { ifMatch: '"1"', key: expect.any(String) });
      expect(runtime.power).not.toHaveBeenCalled();
      expect(runtime.track).toHaveBeenCalledWith('91', '强制清理删除实例');
      expect(runtime.replace).toHaveBeenCalledWith({ query: { tab: 'tasks' } });
      expect(runtime.replace).not.toHaveBeenCalledWith('/instances');
      expect(root.textContent).toContain('private-10-2');
    },
  );
  it.each([false, undefined])('fails closed when force_delete is %s', async (permission) => {
    const value = sample();
    value.capabilities.force_delete = permission;
    runtime.instance.mockResolvedValue({ data: value, etag: '"1"' });
    mount();
    await flush();
    expect(button('强制清理删除')).toBeUndefined();
    expect(runtime.delete).not.toHaveBeenCalled();
  });
  it('requires an exact name and irreversible-loss checkbox in the actual confirmation component', async () => {
    mount();
    await flush();
    button('强制清理删除').click();
    await nextTick();
    const dialog = cleanupDialog()!;
    const confirm = dialog.querySelector<HTMLButtonElement>('[data-confirm]')!;
    expect(confirm.disabled).toBe(true);
    const input = dialog.querySelector<HTMLInputElement>('input:not([type="checkbox"])')!;
    input.value = 'private-10-2 ';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    const risk = dialog.querySelector<HTMLInputElement>('input[type="checkbox"]')!;
    risk.click();
    await nextTick();
    expect(confirm.disabled).toBe(true);
    input.value = 'private-10-2';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    risk.click();
    await nextTick();
    expect(confirm.disabled).toBe(true);
    expect(runtime.delete).not.toHaveBeenCalled();
    risk.click();
    await nextTick();
    expect(confirm.disabled).toBe(false);
    confirm.click();
    await flush();
    expect(runtime.delete).toHaveBeenCalledTimes(1);
  });
  it('retries exactly the unknown cleanup body, key, ETag and name rather than replacing its intent', async () => {
    runtime.delete.mockRejectedValueOnce(new Error('connection lost'));
    mount();
    await flush();
    button('强制清理删除').click();
    await nextTick();
    await confirmCleanup();
    const first = runtime.delete.mock.calls[0];
    expect(button('强制清理删除').disabled).toBe(true);
    runtime.instance.mockResolvedValue({
      data: { ...sample(), name: 'new-name', active_task_id: '90' },
      etag: '"9"',
    });
    button('刷新').click();
    await flush();
    button('重试原请求').click();
    await flush();
    expect(runtime.delete.mock.calls[1]).toEqual(first);
    expect(runtime.replace).not.toHaveBeenCalledWith('/instances');
  });
  it('requires new name/risk confirmation and a fresh request after a stale 412', async () => {
    runtime.delete.mockRejectedValueOnce(
      new ApiError(412, { code: 'revision_conflict', message: '', details: null }, '', null),
    );
    mount();
    await flush();
    button('强制清理删除').click();
    await nextTick();
    runtime.instance.mockResolvedValue({
      data: { ...sample(), name: 'renamed' },
      etag: '"2"',
    });
    await confirmCleanup();
    expect(cleanupDialog()).toBeNull();
    button('强制清理删除').click();
    await nextTick();
    expect(cleanupDialog()!.querySelector<HTMLInputElement>('input')!.value).toBe('');
    expect(cleanupDialog()!.querySelector<HTMLInputElement>('[type="checkbox"]')!.checked).toBe(false);
    expect(cleanupDialog()!.querySelector<HTMLButtonElement>('[data-confirm]')!.disabled).toBe(true);
    await confirmCleanup('renamed');
    expect(runtime.delete.mock.calls[1]?.[1].confirm_name).toBe('renamed');
    expect(runtime.delete.mock.calls[1]?.[2].ifMatch).toBe('"2"');
    expect(runtime.delete.mock.calls[1]?.[2].key).not.toBe(runtime.delete.mock.calls[0]?.[2].key);
  });
  it('keeps cleanup visible for unresolved child requests, blocks actual HTTP submits, and resets old drafts only after acceptance', async () => {
    runtime.route.query = { tab: 'gpu' };
    mount();
    await flush();
    button('gpu unknown').click();
    button('resize unknown').click();
    await nextTick();
    expect(button('强制清理删除').disabled).toBe(false);
    button('gpu submitting').click();
    await nextTick();
    expect(button('强制清理删除').disabled).toBe(true);
    button('gpu settled').click();
    button('resize submitting').click();
    await nextTick();
    expect(button('强制清理删除').disabled).toBe(true);
    button('resize settled').click();
    await nextTick();
    button('强制清理删除').click();
    await nextTick();
    const pending = Promise.withResolvers<unknown>();
    runtime.delete.mockReturnValueOnce(pending.promise);
    await confirmCleanup();
    expect(runtime.gpuMount).toHaveBeenCalledTimes(1);
    expect(runtime.resizeMount).toHaveBeenCalledTimes(1);
    expect(button('强制清理删除').disabled).toBe(true);
    pending.resolve({ data: { task_id: '91', instance_id: '10', status: 'queued' } });
    await flush();
    expect(runtime.gpuMount).toHaveBeenCalledTimes(2);
    expect(runtime.resizeMount).toHaveBeenCalledTimes(2);
    expect(runtime.power).not.toHaveBeenCalled();
    expect(runtime.delete).toHaveBeenCalledTimes(1);
  });
  it('does not replace an unknown local stop with cleanup', async () => {
    runtime.power.mockRejectedValueOnce(new Error('connection lost'));
    mount();
    await flush();
    button('停止').click();
    await nextTick();
    root.querySelector<HTMLButtonElement>('[data-confirm="停止实例"]')!.click();
    await flush();
    expect(button('强制清理删除').disabled).toBe(true);
    expect(runtime.delete).not.toHaveBeenCalled();
  });
  it('does not reuse an unknown cleanup after delete permission is revoked and later restored', async () => {
    runtime.delete.mockRejectedValueOnce(new Error('connection lost'));
    mount();
    await flush();
    button('强制清理删除').click();
    await nextTick();
    await confirmCleanup();
    const denied = sample();
    denied.capabilities.force_delete = false;
    runtime.instance.mockResolvedValue({ data: denied, etag: '"2"' });
    button('刷新').click();
    await flush();
    expect(button('重试原请求').disabled).toBe(true);
    runtime.instance.mockResolvedValue({ data: sample(), etag: '"3"' });
    button('刷新').click();
    await flush();
    expect(button('重试原请求').disabled).toBe(true);
    expect(button('强制清理删除').disabled).toBe(true);
    expect(runtime.delete).toHaveBeenCalledTimes(1);
  });
  it.each(['account', 'instance'] as const)('ignores a late cleanup response after the %s changes', async (change) => {
    const pending = Promise.withResolvers<unknown>();
    runtime.delete.mockReturnValueOnce(pending.promise);
    mount();
    await flush();
    button('强制清理删除').click();
    await nextTick();
    await confirmCleanup();
    runtime.instance.mockResolvedValue({
      data: change === 'account' ? sample('10', '3') : sample('11'),
      etag: '"2"',
    });
    if (change === 'account') runtime.auth.me.user.id = '3';
    else runtime.route.params.id = '11';
    await flush();
    pending.resolve({ data: { task_id: '91', instance_id: '10', status: 'queued' } });
    await flush();
    expect(runtime.track).not.toHaveBeenCalledWith('91', '强制清理删除实例');
    expect(runtime.replace).not.toHaveBeenCalledWith('/instances');
    expect(root.textContent).not.toContain('private-10-2');
  });
  it.each(['404', 'deleted'] as const)('returns to the list only after confirmed %s following acceptance', async (confirmation) => {
    mount();
    await flush();
    button('强制清理删除').click();
    await nextTick();
    await confirmCleanup();
    expect(runtime.replace).not.toHaveBeenCalledWith('/instances');
    if (confirmation === '404')
      runtime.instance.mockRejectedValueOnce(
        new ApiError(404, { code: 'not_found', message: '', details: null }, '', null),
      );
    else
      runtime.instance.mockResolvedValue({
        data: { ...sample(), desired_state: 'deleted', observed_state: 'deleted', provisioning_state: 'deleted' },
        etag: '"2"',
      });
    button('刷新').click();
    await flush();
    expect(runtime.replace).toHaveBeenCalledWith('/instances');
  });
  it('does not mistake a delete desired state without confirmed absence for completed deletion', async () => {
    runtime.instance.mockResolvedValue({
      data: { ...sample(), desired_state: 'deleted', active_task_id: '90', active_task_status: 'unknown' },
      etag: '"1"',
    });
    mount();
    await flush();
    expect(button('强制清理删除').disabled).toBe(false);
    button('强制清理删除').click();
    await nextTick();
    runtime.instance.mockResolvedValue({
      data: { ...sample(), desired_state: 'deleted' },
      etag: '"2"',
    });
    await confirmCleanup();
    expect(runtime.replace).not.toHaveBeenCalledWith('/instances');
    expect(root.textContent).toContain('private-10-2');
  });
  it('blocks fresh cleanup when detail refresh fails rather than trusting stale permission data', async () => {
    mount();
    await flush();
    runtime.instance.mockRejectedValueOnce(new Error('offline'));
    button('刷新').click();
    await flush();
    expect(button('强制清理删除').disabled).toBe(true);
    expect(runtime.delete).not.toHaveBeenCalled();
  });
  it('preserves the normal stopped-only deletion body without force fields', async () => {
    const value = sample();
    value.observed_state = 'stopped';
    value.capabilities.delete = true;
    runtime.instance.mockResolvedValue({ data: value, etag: '"1"' });
    runtime.gpuPolicy.mockResolvedValue({ data: { allowed_user_actions: ['delete'] } });
    mount();
    await flush();
    [...root.querySelectorAll<HTMLElement>('div')].find(
      (element) => element.textContent === '销毁实例（需先停止）',
    )!.click();
    await nextTick();
    const dialog = root.querySelector<HTMLElement>('[data-dialog="销毁实例"]')!;
    expect(dialog.querySelector('[type="checkbox"]')).toBeNull();
    const input = dialog.querySelector<HTMLInputElement>('input')!;
    input.value = value.name;
    input.dispatchEvent(new Event('input', { bubbles: true }));
    await nextTick();
    dialog.querySelector<HTMLButtonElement>('[data-confirm]')!.click();
    await flush();
    expect(runtime.delete).toHaveBeenCalledWith('10',
      { confirm_name: value.name, delete_data: true },
      { ifMatch: '"1"', key: expect.any(String) });
  });
});
