import type { App, Slots } from 'vue';
import type { CatalogRefs } from '../refs';
import type { Plan } from '#/api/types';

import { createApp, defineComponent, h, nextTick, ref } from 'vue';
import { afterEach, describe, expect, it, vi } from 'vitest';

import PlanDrawer from './plan-drawer.vue';

const api = vi.hoisted(() => ({ plan: vi.fn(), updatePlan: vi.fn() }));
vi.mock('#/api/backend', () => ({ catalogApi: api }));
vi.mock('#/composables/api', () => {
  return {
    errorToast: vi.fn(),
    useAction: () => ({
      pending: ref(false), resetKey: vi.fn(),
      run: (action: (key: string) => Promise<unknown>) => action('test-key'),
    }),
    useResource: (load: () => Promise<{ data: Plan; etag: string }>) => {
      const data = ref<Plan>();
      const etag = ref('');
      return { data, etag, loading: ref(false), reload: async () => {
        const result = await load(); data.value = result.data; etag.value = result.etag;
      } };
    },
  };
});
vi.mock('ant-design-vue', () => {
  const container = { setup: (_: unknown, { slots }: { slots: Slots }) => () => h('div', [slots.extra?.(), slots.default?.(), slots.description?.()]) };
  const input = {
    props: ['value', 'disabled'], emits: ['update:value'],
    setup: (props: { value?: string; disabled?: boolean }, { emit }: { emit: (event: string, value: string) => void }) => () => h('input', {
      value: props.value, disabled: props.disabled,
      onInput: (event: Event) => emit('update:value', (event.target as HTMLInputElement).value),
    }),
  };
  return {
    Alert: container, AutoComplete: input, CheckboxGroup: container, Divider: container,
    Drawer: container, Form: container, Input: input, InputNumber: input,
    RadioButton: container, RadioGroup: container, Select: container, Space: container, Switch: container,
    Button: { setup: (_: unknown, { slots }: { slots: Slots }) => () => h('button', slots.default?.()) },
    FormItem: { props: ['label'], setup: (props: { label: string }, { slots }: { slots: Slots }) => () => h('label', { 'data-field': props.label }, slots.default?.()) },
  };
});

let app: App | undefined;
let host: HTMLDivElement | undefined;
afterEach(() => { app?.unmount(); host?.remove(); vi.clearAllMocks(); });

async function flush() { for (let index = 0; index < 6; index++) { await Promise.resolve(); await nextTick(); } }

describe('plan editor exact Pt round trip', () => {
  it('keeps a six-decimal GPU cap in the input and both submitted caps after an unrelated name edit', async () => {
    const kinds = ['pricing', 'credit', 'metrics', 'gpu-idle', 'gpu-scheduling'] as const;
    const policies = Object.fromEntries(kinds.map((kind) => [kind, [{
      id: kind, code: kind, version: '1', kind, status: 'active',
      config: kind === 'gpu-idle' ? { min_timeout_seconds: 60, max_timeout_seconds: 3600, allowed_actions: ['release_gpu'] } : {},
    }]]));
    const plan = {
      id: '71', code: 'basic', version: '1', name: 'Original', instance_type: 'container',
      resources: { vcpus: 2, memory_bytes: '4294967296', disk_bytes: '10737418240' },
      gpu: { count: 0, model: null, access_mode: 'exclusive', max_hourly_pt: '1.049999' },
      gpu_policy: { idle: { action: 'detach_gpu', enabled: false, timeout_seconds: 300 }, rebalance: { enabled: false, allow_live: false, max_hourly_pt: '1.049999' } },
      gpu_user_permissions: {}, allowed_overrides: {}, allowed_user_actions: ['start'],
      image_ids: ['image'], node_ids: ['node'], network_option_ids: ['binding'],
      policy_versions: Object.fromEntries(kinds.map((kind) => [kind, kind])),
      init_template_version_id: null,
    } as unknown as Plan;
    const refs = {
      bindings: ref([{ id: 'binding', node_id: 'node', status: 'active', mode: 'bridge', project_name: 'default', validation_status: 'verified' }]),
      nodes: ref([{ id: 'node', name: 'Node', administrative_state: 'enabled', health: 'online' }]),
      images: ref([{ id: 'image', name: 'Image', instance_type: 'container', status: 'active', nodes: [{ node_id: 'node', project_name: 'default', status: 'ready', architecture: 'x86_64' }] }]),
      templates: ref([]), policies, bindingName: () => 'Network', nodeName: () => 'Node', templateName: () => 'Template',
    } as unknown as CatalogRefs;
    api.plan.mockResolvedValue({ data: plan, etag: '"7"' });
    api.updatePlan.mockResolvedValue({ data: plan });
    const open = ref(false);
    app = createApp(defineComponent({ setup: () => () => h(PlanDrawer, { open: open.value, mode: 'edit', source: plan, refs }) }));
    host = document.createElement('div'); document.body.append(host); app.mount(host);
    open.value = true; await flush();
    const cap = host.querySelector<HTMLInputElement>('[data-field="GPU 分配 / 均衡单价上限"] input')!;
    expect(cap.value).toBe('1.049999');
    const name = host.querySelector<HTMLInputElement>('[data-field="名称"] input')!;
    name.value = 'Renamed'; name.dispatchEvent(new Event('input', { bubbles: true })); await flush();
    [...host.querySelectorAll('button')].find((button) => button.textContent?.trim() === '保存套餐')!.click(); await flush();
    expect(api.updatePlan).toHaveBeenCalledWith('71', expect.objectContaining({
      name: 'Renamed', gpu: expect.objectContaining({ max_hourly_pt: '1.049999' }),
      gpu_policy: expect.objectContaining({ rebalance: expect.objectContaining({ max_hourly_pt: '1.049999' }) }),
    }), { ifMatch: '"7"', key: 'test-key' });
    expect(plan.gpu.max_hourly_pt).toBe('1.049999');
  });
});
