import type { App, Slots } from 'vue';
import type { Instance } from '#/api/types';

import { createApp, h, nextTick, reactive, ref } from 'vue';

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { ApiError } from '#/api/request';

const runtime = vi.hoisted(() => ({
  access: { loginExpired: false }, auth: { me: { user: { id: 'user-a', role: 'user' } } },
  route: { query: {} as Record<string, string> },
  instance: vi.fn(), instances: vi.fn(), push: vi.fn(), replace: vi.fn(),
}));
vi.mock('@vben/stores', () => ({ useAccessStore: () => runtime.access }));
vi.mock('#/store/auth', () => ({ useAuthStore: () => runtime.auth }));
vi.mock('#/store/tasks', () => ({ useTaskStore: () => ({ tracked: [] }) }));
vi.mock('vue-router', () => ({ useRoute: () => runtime.route, useRouter: () => ({ push: runtime.push, replace: runtime.replace }) }));
vi.mock('#/api/backend', () => ({ memberApi: { instance: runtime.instance, instances: runtime.instances } }));
vi.mock('#/composables/api', () => ({
  usePolling: () => undefined,
  useCursorList: (fetch: (query: Record<string, unknown>) => Promise<{ data: { items: Instance[] } }>) => {
    const items = ref<Instance[]>([]);
    const loading = ref(false);
    return {
      items, loading, page: () => 1, hasNext: () => false, hasPrev: () => false,
      nextPage: vi.fn(), prevPage: vi.fn(), reset: () => { items.value = []; },
      load: async () => {
        loading.value = true;
        try { items.value = (await fetch({})).data.items; } finally { loading.value = false; }
      },
    };
  },
}));
vi.mock('#/components/cursor-pager.vue', () => ({ default: () => null }));
vi.mock('#/components/error-feedback.vue', () => ({ default: {
  props: ['feedback'], setup: () => () => h('p', { role: 'alert' }, '实例不可用'),
} }));
vi.mock('./components/instance-card.vue', () => ({ default: {
  props: ['instance'],
  setup: (props: { instance: Instance }) => () => h('article', { 'data-instance': props.instance.id }, props.instance.name),
} }));
vi.mock('@vben/common-ui', () => ({ Page: { setup: (_: unknown, { slots }: { slots: Slots }) => () => h('main', slots.default?.()) } }));
vi.mock('ant-design-vue', () => {
  const container = { setup: (_: unknown, { slots }: { slots: Slots }) => () => h('div', slots.default?.()) };
  return {
    Alert: container, Button: 'button', Card: container, Select: () => null, Skeleton: () => null,
    Spin: () => h('span', '正在读取实例详情'),
    Empty: { props: ['description'], setup: (props: { description: string }) => () => h('p', props.description) },
    Drawer: { props: ['open'], setup: (props: { open: boolean }, { slots }: { slots: Slots }) => () => props.open ? h('section', { role: 'dialog' }, slots.default?.()) : null },
  };
});

import Instances from './index.vue';

let app: App;
let root: HTMLElement;
async function flush() {
  for (let i = 0; i < 8; ++i) await Promise.resolve();
  await nextTick();
}
function mount() {
  root = document.createElement('div');
  document.body.append(root);
  app = createApp(Instances);
  app.mount(root);
}
const unavailable = () => new ApiError(404, { code: 'not_found', message: '', details: null }, '', null);
function instance(id: string) {
  return { id, user_id: 'user-a', name: `private-name-${id}`, provisioning_state: 'failed',
    observed_state: 'unknown', credit_enforcement: { status: 'normal' } };
}
beforeEach(() => {
  runtime.instance.mockReset();
  runtime.instances.mockReset();
  runtime.route = reactive({ query: {} });
  runtime.auth = reactive({ me: { user: { id: 'user-a', role: 'user' } } });
  runtime.access = reactive({ loginExpired: false });
  runtime.instances.mockResolvedValue({ data: { items: [] } });
  runtime.instance.mockRejectedValue(unavailable());
});
afterEach(() => { app?.unmount(); root?.remove(); });

describe('member instance list', () => {
  it('renders the backend member list without inferring failed-creation visibility from provisioning state', async () => {
    runtime.instances.mockResolvedValue({ data: { items: [instance('backend-visible')] } });
    mount();
    await flush();
    expect(root.querySelector('[data-instance="backend-visible"]')).not.toBeNull();
    expect(runtime.instance).not.toHaveBeenCalled();
  });
});
