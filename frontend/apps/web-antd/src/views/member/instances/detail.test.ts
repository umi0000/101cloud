import type { App, Slots } from 'vue';
import type { Instance } from '#/api/types';
import { createApp, h, nextTick, reactive } from 'vue';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiError } from '#/api/request';

const runtime = vi.hoisted(() => ({
  auth: { me: { user: { id: '2', role: 'user' } } }, access: { loginExpired: false },
  route: { params: { id: '10' }, query: {} as Record<string, string> },
  instance: vi.fn(), network: vi.fn(), gpuAllocations: vi.fn(), gpuPolicy: vi.fn(), power: vi.fn(),
  track: vi.fn(), replace: vi.fn(), push: vi.fn(),
}));
vi.mock('@vben/stores', () => ({ useAccessStore: () => runtime.access }));
vi.mock('#/store/auth', () => ({ useAuthStore: () => runtime.auth }));
vi.mock('#/store/tasks', () => ({ useTaskStore: () => ({ tracked: [], track: runtime.track }) }));
vi.mock('vue-router', () => ({ onBeforeRouteLeave: vi.fn(), useRoute: () => runtime.route, useRouter: () => ({ replace: runtime.replace, push: runtime.push }) }));
vi.mock('#/api/backend', () => ({ memberConsoleApi: {}, memberApi: { instance: runtime.instance, network: runtime.network, gpuAllocations: runtime.gpuAllocations, gpuPolicy: runtime.gpuPolicy, power: runtime.power } }));
vi.mock('#/composables/api', () => ({ usePolling: () => undefined }));
vi.mock('@vben/common-ui', () => ({ Page: { setup: (_: unknown, { slots }: { slots: Slots }) => () => h('main', slots.default?.()) } }));
vi.mock('ant-design-vue', () => {
  const container = { setup: (_: unknown, { slots }: { slots: Slots }) => () => h('div', [slots.default?.(), slots.action?.(), slots.overlay?.()]) };
  return { ...Object.fromEntries(['Alert','Card','Dropdown','Menu','MenuItem','Modal','TabPane','Tabs','TypographyText'].map(k => [k, container])),
    Button: 'button', Spin: () => null, Tag: 'span', Input: 'input',
    Empty: { props: ['description'], setup: (p: { description: string }) => () => h('p', { 'data-empty': '' }, p.description) },
  };
});
vi.mock('#/components/confirm-action.vue', () => ({ default: {
  props: ['open','title','disabled','onConfirm'],
  setup: (p: { open: boolean; title: string; disabled: boolean; onConfirm: () => unknown }) => () => p.open ? h('button', { 'data-confirm': p.title, disabled: p.disabled, onClick: p.onConfirm }, p.title) : null,
} }));
vi.mock('#/components/error-feedback.vue', () => ({ default: { props: ['feedback'], setup: () => () => h('span', { role: 'alert' }, 'error') } }));
vi.mock('#/components/os-icon.vue', () => ({ default: () => null }));
vi.mock('#/components/status-tag.vue', () => ({ default: () => null }));
vi.mock('#/components/time-text.vue', () => ({ default: () => null }));
vi.mock('#/components/instances/console-drawer.vue', () => ({ default: () => null }));
vi.mock('#/components/instances/resize-dialog.vue', () => ({ default: () => null }));
vi.mock('./components/gpu-config-panel.vue', () => ({ default: () => null }));
vi.mock('./components/metrics-panel.vue', () => ({ default: () => null }));
vi.mock('./components/task-panel.vue', () => ({ default: () => null }));
import Detail from './detail.vue';
let app: App; let root: HTMLElement;
async function flush() { for (let n = 0; n < 20; n++) await Promise.resolve(); await nextTick(); }
function sample(id = '10', owner = '2'): Instance {
  return { id, user_id: owner, name: `private-${id}-${owner}`, instance_type: 'container', product_type: 'elastic',
    observed_state: 'running', desired_state: 'running', provisioning_state: 'ready', state_stale: false,
    active_task_id: null, resources: { vcpus: 4, memory_bytes: '8589934592', disk_bytes: '42949672960' },
    credit_enforcement: { status: 'normal' }, capabilities: { console: true, start: false, stop: true, resize: true, delete: false },
  } as Instance;
}
function mount() { root = document.createElement('div'); document.body.append(root); app = createApp(Detail); app.mount(root); }
const button = (label: string) => [...root.querySelectorAll<HTMLButtonElement>('button')].find(b => b.textContent === label)!;
beforeEach(() => {
  vi.clearAllMocks();
  runtime.route = reactive({ params: { id: '10' }, query: {} });
  runtime.auth = reactive({ me: { user: { id: '2', role: 'user' } } });
  runtime.access = reactive({ loginExpired: false });
  runtime.instance.mockResolvedValue({ data: sample(), etag: '"1"' });
  runtime.network.mockResolvedValue({ data: [] }); runtime.gpuAllocations.mockResolvedValue({ data: [] });
  runtime.gpuPolicy.mockResolvedValue({ data: { allowed_user_actions: ['stop', 'restart'] } });
});
afterEach(() => { app?.unmount(); root?.remove(); });

describe('member instance detail transitions', () => {
  it('never renders another owner response and clears old details before a route change', async () => {
    mount(); await flush(); expect(root.textContent).toContain('private-10-2');
    const pending = Promise.withResolvers<unknown>(); runtime.instance.mockReturnValueOnce(pending.promise);
    runtime.route.params.id = '11'; await nextTick(); expect(root.textContent).not.toContain('private-10-2');
    pending.resolve({ data: sample('11', '3'), etag: '"2"' }); await flush();
    expect(root.textContent).not.toContain('private-11-3'); expect(root.querySelector('[data-empty]')).not.toBeNull();
  });
  it('discards an old account response after switching accounts', async () => {
    const old = Promise.withResolvers<unknown>(); runtime.instance.mockReturnValueOnce(old.promise);
    mount(); runtime.instance.mockResolvedValue({ data: sample('10', '3'), etag: '"2"' });
    runtime.auth.me.user.id = '3'; await flush(); old.resolve({ data: sample(), etag: '"1"' }); await flush();
    expect(root.textContent).toContain('private-10-3'); expect(root.textContent).not.toContain('private-10-2');
  });
  it('removes a hidden or removed instance rather than retaining its previous configuration', async () => {
    mount(); await flush(); runtime.instance.mockRejectedValueOnce(new ApiError(404, { code: 'not_found', message: '', details: null }, '', null));
    button('刷新').click(); await flush(); expect(root.textContent).not.toContain('private-10-2');
    expect(root.querySelector('[data-empty]')).not.toBeNull();
  });
  it('does not expose zero GPU or empty networking before independent resources resolve', async () => {
    const gpu = Promise.withResolvers<unknown>(); const net = Promise.withResolvers<unknown>();
    runtime.gpuAllocations.mockReturnValue(gpu.promise); runtime.network.mockReturnValue(net.promise);
    mount(); await flush(); expect(root.textContent).not.toContain('0 张');
    expect(root.querySelectorAll('[data-empty]')).toHaveLength(0);
    gpu.resolve({ data: [] }); net.resolve({ data: [] }); await flush();
    expect(root.textContent).toContain('0 张'); expect(root.querySelectorAll('[data-empty]').length).toBeGreaterThan(0);
  });
  it('freezes the original request and ETag after an uncertain stop while disabling new actions', async () => {
    runtime.power.mockRejectedValueOnce(new ApiError(503, { code: 'unavailable', message: '', details: null }, '', null));
    mount(); await flush(); button('停止').click(); await nextTick();
    root.querySelector<HTMLButtonElement>('[data-confirm="停止实例"]')!.click(); await flush();
    const first = runtime.power.mock.calls[0]; expect(first?.[2]).toEqual({ mode: 'graceful' });
    expect(button('停止').disabled).toBe(true);
    runtime.instance.mockResolvedValue({ data: { ...sample(), active_task_id: '90' }, etag: '"9"' });
    button('刷新').click(); await flush();
    runtime.power.mockResolvedValueOnce({ data: { task_id: '90', instance_id: '10', status: 'queued' } });
    button('重试原请求').click(); await flush();
    expect(runtime.power.mock.calls[1]).toEqual(first);
    expect(runtime.track).toHaveBeenCalledWith('90', '停止实例');
  });
  it('disables lifecycle changes when the instance state is stale', async () => {
    runtime.instance.mockResolvedValue({ data: { ...sample(), state_stale: true }, etag: '"1"' });
    mount(); await flush(); expect(button('停止').disabled).toBe(true); expect(runtime.power).not.toHaveBeenCalled();
  });
});
