import type { App, Slots } from 'vue';

import { createApp, h, nextTick, reactive, ref, shallowRef } from 'vue';

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { ApiError } from '#/api/request';
import { POLICY_KINDS } from '#/api/types';
import Index from './index.vue';

const runtime = vi.hoisted(() => ({
  create: vi.fn(), images: vi.fn(), track: vi.fn(), toast: vi.fn(),
  auth: {} as Record<string, any>, access: {} as Record<string, any>,
}));
vi.mock('vue-router', () => ({ useRoute: () => ({ query: {} }) }));
vi.mock('@vben/stores', () => ({ useAccessStore: () => runtime.access }));
vi.mock('#/store', () => ({ useAuthStore: () => runtime.auth }));
vi.mock('#/store/user-cache', () => ({ useUserCache: () => ({ put: vi.fn() }) }));
vi.mock('#/api/backend', () => ({
  instancesApi: { createCustom: runtime.create, list: vi.fn() },
  usersApi: { list: async () => [{ id: 'owner', username: 'Owner', status: 'active' }] },
  nodesApi: {
    list: async () => [{ id: 'node-a', name: 'Node A', administrative_state: 'active', health: 'online', defaults: { project_name: 'default' } }],
    gpus: async () => [],
  },
  catalogApi: {
    images: runtime.images, templates: async () => [], gpuOptions: async () => [],
    networkOptions: async () => [{ id: 'net', node_id: 'node-a', project_name: 'default', mode: 'bridge' }],
  },
  policiesApi: { list: async (kind: string) => [{ id: kind, code: kind, kind, version: 1, status: 'active', config: {} }] },
  listAll: (fn: (query?: unknown) => Promise<unknown>, query?: unknown) => fn(query),
}));
vi.mock('#/composables/api', () => ({
  errorToast: runtime.toast, trackAccepted: runtime.track, usePolling: vi.fn(),
  useCursorList: () => ({
    items: shallowRef([]), loading: ref(false), page: () => 1, load: vi.fn(),
    hasNext: () => false, hasPrev: () => false,
  }),
}));
vi.mock('@vben/common-ui', () => ({ Page: { setup: (_: unknown, { slots }: { slots: Slots }) => () => h('main', [slots.extra?.(), slots.default?.()]) } }));
// Native controls exercise the actual page/form watchers, validation and submit state machine.
vi.mock('ant-design-vue', () => {
  const container = { setup: (_: unknown, { slots }: { slots: Slots }) => () => h('div', slots.default?.()) };
  const input = (tag: string, type = 'text') => ({
    props: ['value'], emits: ['update:value'],
    setup: (props: Record<string, any>, { emit }: { emit: (event: string, value: unknown) => void }) => () => h(tag, {
      type, value: props.value, onInput: (event: Event) => emit('update:value', (event.target as HTMLInputElement).value),
    }),
  });
  return {
    ...Object.fromEntries(['Card', 'Col', 'Row', 'Space', 'Table', 'Tag', 'CheckboxGroup', 'Switch', 'RadioButton'].map((key) => [key, container])),
    Button: 'button', Input: input('input'), InputPassword: input('input', 'password'), Textarea: input('textarea'), InputNumber: input('input', 'number'),
    message: { success: vi.fn() },
    Alert: { props: ['message', 'description'], setup: (props: Record<string, any>) => () => h('aside', [props.message, props.description]) },
    Drawer: { props: ['open'], setup: (props: Record<string, any>, { slots }: { slots: Slots }) => () => props.open ? h('section', { role: 'dialog' }, [slots.default?.(), slots.footer?.()]) : null },
    Form: { props: ['disabled'], setup: (props: Record<string, any>, { slots }: { slots: Slots }) => () => h('fieldset', { disabled: props.disabled }, slots.default?.()) },
    FormItem: { props: ['label', 'help'], setup: (props: Record<string, any>, { slots }: { slots: Slots }) => () => h('label', { 'data-label': props.label }, [props.label, slots.default?.(), props.help]) },
    Select: {
      props: ['value', 'options'], emits: ['update:value'],
      setup: (props: Record<string, any>, { emit }: { emit: (event: string, value: unknown) => void }) => () => h('select', {
        value: props.value ?? '', onChange: (event: Event) => emit('update:value', (event.target as HTMLSelectElement).value || undefined),
      }, [h('option', { value: '' }, '请选择'), ...(props.options ?? []).map((option: Record<string, any>) => h('option', { value: option.value, disabled: option.disabled }, option.label))]),
    },
    RadioGroup: {
      props: ['value'], emits: ['update:value'],
      setup: (props: Record<string, any>, { emit, slots }: { emit: (event: string, value: unknown) => void; slots: Slots }) => () => h('select', {
        value: props.value, onChange: (event: Event) => emit('update:value', (event.target as HTMLSelectElement).value),
      }, slots.default?.().filter((node) => node.props?.value).map((node) => h('option', { value: node.props!.value }, node.props!.value))),
    },
  };
});
vi.mock('#/components/cursor-pager.vue', () => ({ default: () => null }));
vi.mock('#/components/status-tag.vue', () => ({ default: () => null }));
vi.mock('#/components/time-text.vue', () => ({ default: () => null }));
vi.mock('#/components/user-name.vue', () => ({ default: () => null }));
vi.mock('./components/batch-instance-actions.vue', () => ({ default: () => null }));
vi.mock('./components/grace-tag.vue', () => ({ default: () => null }));

let app: App;
let root: HTMLElement;
const password = '  admin-secret🙂  ';
const publicKey = `ssh-ed25519 ${btoa('\0\0\0\x0bssh-ed25519\0\0\0\x20' + 'x'.repeat(32))}`;
const image = (cloudInit: boolean, project = 'default') => ({
  id: 'image', name: 'Image', status: 'active', instance_type: 'container', os_name: 'Linux', os_release: '1', architecture: 'x86_64',
  cloud_init: true, nodes: [{ node_id: 'node-a', project_name: project, status: 'ready', cloud_init: cloudInit }],
});
async function flush() {
  for (let count = 0; count < 20; count++) await Promise.resolve();
  await nextTick();
}
function button(label: string) {
  const element = [...root.querySelectorAll('button')].find((item) => item.textContent === label);
  if (!element) throw new Error(`Missing button ${label}`);
  return element;
}
async function click(label: string) { button(label).click(); await flush(); }
async function field(selector: string, value: string) {
  const element = root.querySelector(selector) as HTMLInputElement | HTMLSelectElement;
  if (!element) throw new Error(`Missing field ${selector}`);
  element.value = value;
  element.dispatchEvent(new Event(element.tagName === 'SELECT' ? 'change' : 'input', { bubbles: true }));
  await flush();
}
async function mount() {
  root = document.createElement('div');
  document.body.append(root);
  app = createApp(Index);
  app.component('RouterLink', { setup: (_: unknown, { slots }: { slots: Slots }) => () => h('a', slots.default?.()) });
  app.mount(root);
  await flush();
  await click('直接下发自定义实例');
}
async function configure() {
  await field('#custom-owner', 'owner');
  await field('#custom-name', 'custom-one');
  await field('[data-label="节点"] select', 'node-a');
  await field('[data-label="网络"] select', 'net');
  await field('[data-label="镜像"] select', 'image');
  for (const select of root.querySelectorAll<HTMLSelectElement>('select')) {
    const option = [...select.options].find((item) => POLICY_KINDS.includes(item.value as typeof POLICY_KINDS[number]));
    if (option) {
      select.value = option.value;
      select.dispatchEvent(new Event('change', { bubbles: true }));
      await flush();
    }
  }
}
async function setPassword() {
  await field('#custom-root-password', password);
  await field('#custom-root-password-confirmation', password);
}
beforeEach(() => {
  vi.clearAllMocks();
  runtime.auth = reactive({ me: { user: { id: 'admin-a', role: 'admin' } } });
  runtime.access = reactive({ loginExpired: false });
  runtime.images.mockResolvedValue([image(true)]);
  runtime.create.mockResolvedValue({ data: { task_id: 'task', instance_id: 'instance' } });
});
afterEach(() => { app?.unmount(); root?.remove(); });

describe('administrator custom instance login', () => {
  it('requires confirmed credentials and submits password only at the top level, then clears on acceptance', async () => {
    await mount(); await configure();
    expect(button('下发').disabled).toBe(true);
    await field('#custom-root-password', password);
    expect(button('下发').disabled).toBe(true);
    await field('#custom-root-password-confirmation', 'mismatch');
    expect(root.textContent).toContain('两次输入的 root 密码不一致');
    await field('#custom-root-password-confirmation', password);
    expect(button('下发').disabled).toBe(false);
    await click('下发');
    const body = runtime.create.mock.calls[0]![0];
    expect(body.login).toEqual({ root_password: password });
    expect(body.approved_config).not.toHaveProperty('login');
    expect(body.approved_config.init_parameters).toEqual({});
    expect(JSON.stringify(body.approved_config)).not.toContain(password);
    expect(runtime.track).toHaveBeenCalledOnce();
    expect(root.querySelector('[role="dialog"]')).toBeNull();
    await click('直接下发自定义实例');
    expect((root.querySelector('#custom-root-password') as HTMLInputElement).value).toBe('');
  });

  it('clears inactive secrets on mode changes and accepts normalized public keys without a password', async () => {
    await mount(); await configure(); await setPassword();
    await field('#custom-login-mode', 'ssh-key');
    expect(button('下发').disabled).toBe(true);
    await field('#custom-ssh-public-keys', '-----BEGIN OPENSSH PRIVATE KEY-----');
    expect(button('下发').disabled).toBe(true);
    await field('#custom-ssh-public-keys', publicKey);
    await field('#custom-login-mode', 'password');
    expect((root.querySelector('#custom-root-password') as HTMLInputElement).value).toBe('');
    expect((root.querySelector('#custom-root-password-confirmation') as HTMLInputElement).value).toBe('');
    await field('#custom-login-mode', 'ssh-key');
    expect((root.querySelector('#custom-ssh-public-keys') as HTMLTextAreaElement).value).toBe('');
    await field('#custom-ssh-public-keys', `${publicKey} laptop\n${publicKey} desktop`);
    expect(button('下发').disabled).toBe(false);
    await click('下发');
    expect(runtime.create.mock.calls[0]![0].login).toEqual({ ssh_public_keys: [publicKey] });
  });

  it.each([image(false), image(true, 'other-project')])('blocks a selected image without exact node/project cloud-init capability', async (catalogImage) => {
    runtime.images.mockResolvedValue([catalogImage]);
    await mount(); await configure(); await setPassword();
    expect(root.textContent).toContain('所选节点 / 项目的镜像不支持 cloud-init');
    expect(button('下发').disabled).toBe(true);
    expect(runtime.create).not.toHaveBeenCalled();
    expect(runtime.images).toHaveBeenCalledWith({ instance_type: 'container', node_id: 'node-a', project_name: 'default' });
  });

  it.each(['password', 'ssh-key'])('freezes the original %s body/key on unknown outcome and never replays changed fields', async (mode) => {
    runtime.create.mockRejectedValueOnce(new ApiError(0, undefined, '', null));
    await mount(); await configure();
    if (mode === 'password') await setPassword();
    else { await field('#custom-login-mode', 'ssh-key'); await field('#custom-ssh-public-keys', publicKey); }
    await click('下发');
    expect(root.textContent).toContain('下发结果待确认');
    expect([...root.querySelectorAll('fieldset')].every((item) => item.disabled)).toBe(true);
    const [original, write] = runtime.create.mock.calls[0]!;
    // Even programmatic events bypassing disabled DOM controls cannot change the frozen intent.
    await field('#custom-name', 'changed-name');
    await field('#custom-login-mode', mode === 'password' ? 'ssh-key' : 'password');
    await click('重试原始下发');
    expect(runtime.create.mock.calls[1]).toEqual([original, write]);
    expect(runtime.create.mock.calls[1]![0].name).toBe('custom-one');
    expect(runtime.track).toHaveBeenCalledOnce();
  });

  it('unlocks a deterministic rejection, strips unsafe error fields and uses a new intent key', async () => {
    runtime.create.mockRejectedValueOnce(new ApiError(422, { code: 'invalid_request', message: password, details: { password } }, 'request-id', null));
    await mount(); await configure(); await setPassword(); await click('下发');
    expect(root.textContent).not.toContain('下发结果待确认');
    expect([...root.querySelectorAll('fieldset')].every((item) => !item.disabled)).toBe(true);
    const safeError = runtime.toast.mock.calls[0]![0] as ApiError;
    expect(safeError.message).not.toContain(password);
    expect(safeError.details).toBeNull();
    await field('#custom-name', 'corrected'); await click('下发');
    expect(runtime.create.mock.calls[1]![0].name).toBe('corrected');
    expect(runtime.create.mock.calls[1]![1].key).not.toBe(runtime.create.mock.calls[0]![1].key);
  });

  it('disables pending edits and ignores accepted responses from a previous authentication identity', async () => {
    const { promise, resolve: accept } = Promise.withResolvers<unknown>();
    runtime.create.mockImplementationOnce(() => promise);
    await mount(); await configure(); await setPassword(); await click('下发');
    expect([...root.querySelectorAll('fieldset')].every((item) => item.disabled)).toBe(true);
    expect(button('关闭').disabled).toBe(true);
    runtime.auth.me.user.id = 'admin-b'; await flush();
    expect(root.querySelector('[role="dialog"]')).toBeNull();
    accept({ data: { task_id: 'old-task' } }); await flush();
    expect(runtime.track).not.toHaveBeenCalled();
    await click('直接下发自定义实例');
    expect((root.querySelector('#custom-root-password') as HTMLInputElement).value).toBe('');
  });

  it('clears credentials and unknown intents on close and authentication expiry', async () => {
    runtime.create.mockRejectedValueOnce(new ApiError(500, undefined, '', null));
    await mount(); await configure(); await setPassword(); await click('下发');
    await click('关闭'); await click('直接下发自定义实例');
    expect((root.querySelector('#custom-root-password') as HTMLInputElement).value).toBe('');
    expect(root.textContent).not.toContain('下发结果待确认');
    await setPassword();
    runtime.access.loginExpired = true; await flush();
    expect(root.querySelector('[role="dialog"]')).toBeNull();
  });
});
