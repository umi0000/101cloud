import type { InstanceOrderSelection } from './instance-order';

import { reactive } from 'vue';

import { createPinia, setActivePinia } from 'pinia';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import { ApiError } from '#/api/request';
import { emptyLoginDraft } from '#/utils/instance-login';

import { useInstanceLoginDraft, useInstanceOrderStore } from './instance-order';

const runtime = vi.hoisted(() => ({
  auth: null as null | { me: null | { user: { id: string; role: string } } },
  access: null as null | { loginExpired: boolean },
  create: vi.fn(), quote: vi.fn(), credit: vi.fn(), track: vi.fn(), key: 0,
}));

vi.mock('@vben/stores', () => ({ useAccessStore: () => runtime.access }));
vi.mock('./auth', () => ({ useAuthStore: () => runtime.auth }));
vi.mock('./tasks', () => ({ useTaskStore: () => ({ track: runtime.track }) }));
vi.mock('#/api/backend', () => ({ memberApi: { create: runtime.create, quote: runtime.quote, creditAccount: runtime.credit } }));
vi.mock('#/api/request', () => ({
  newIdempotencyKey: () => `key-${++runtime.key}`,
  ApiError: class extends Error {
    code: string;
    details: unknown;
    constructor(public status: number, body: { code: string; details: unknown; message: string }, public requestId: string, public retryAfter: null | number) {
      super(body.message);
      this.code = body.code;
      this.details = body.details;
    }
  },
}));
vi.mock('#/locales/errors', () => ({
  describeError: (error: ApiError) => ({ code: error.code, details: error.details ? [error.message] : [], requestId: error.requestId, stage: '', text: error.message || '安全错误提示' }),
}));

function selection(): InstanceOrderSelection & { account: NonNullable<InstanceOrderSelection['account']> } {
  const time = '2026-11-03T00:00:00Z';
  return {
    spec: {
      name: 'my-instance', image_id: 'image-a', plan_id: 'plan-a', plan_revision: 'rev-1', network_option_id: 'net-a', init_parameters: {},
      gpu: { count: 0, model: null, access_mode: 'shared', max_hourly_pt: '0' },
      resources: { vcpus: 1, memory_bytes: '1073741824', disk_bytes: '10737418240' },
    },
    plan: {
      id: 'plan-a', code: 'container-small', name: 'Small container', version: '1', revision: 'rev-1',
      status: 'active', instance_type: 'container', created_at: time, updated_at: time,
      resources: { vcpus: 1, memory_bytes: '1073741824', disk_bytes: '10737418240' },
      gpu: { count: 0, model: null, access_mode: 'shared', max_hourly_pt: '0' },
      gpu_policy: {
        idle: { action: 'detach_gpu', enabled: false, timeout_seconds: 3600 },
        rebalance: { allow_live: false, enabled: false, max_hourly_pt: '0' },
      },
      gpu_user_permissions: {}, allowed_overrides: {}, allowed_user_actions: ['start', 'stop'],
      image_ids: ['image-a'], network_option_ids: ['net-a'], node_ids: ['node-a'],
      policy_versions: { credit: 'c1', metrics: 'm1', pricing: 'p1' }, pricing_version_id: 'p1', init_template_version_id: null,
    },
    image: {
      id: 'image-a', code: 'ubuntu', name: 'Ubuntu 24.04 cloud', version: '1', revision: 'image-rev-1',
      architecture: 'x86_64', fingerprint: 'a'.repeat(64), instance_type: 'container', status: 'active',
      cloud_init: true, gpu_ready: false, source_type: 'remote', os_name: 'Ubuntu', os_release: '24.04',
      properties: { gpu_ready: false, os_name: 'Ubuntu', os_release: '24.04' },
      created_at: time, updated_at: time,
      nodes: [{
        id: 'image-node-a', node_id: 'node-a', project_name: 'default', architecture: 'x86_64',
        fingerprint: 'a'.repeat(64), cloud_init: true, gpu_ready: false, status: 'ready',
        revision: 'image-node-rev-1', source_id: 'ubuntu', error: null, last_verified_at: time, task_id: null,
      }],
    },
    network: { id: 'net-a', node_id: 'node-a', project_name: 'default', mode: 'managed_bridge', created_at: time },
    account: {
      id: 'account-a', user_id: 'user-a', status: 'active', available_pt: '10', remaining_pt: '10',
      reserved_pt: '0', outstanding_debt_pt: '0', enforcements: [], policy_version_id: 'c1',
      settlement_pending: false,
      revision: 'account-rev-1', server_time: time, next_grant_at: '2026-11-04T00:00:00Z',
    },
    quote: {
      admissible: true, blocking_reasons: [], expires_at: new Date(Date.now() + 60_000).toISOString(), plan_revision: 'rev-1',
      policy_versions: { credit: 'c1', metrics: 'm1', pricing: 'p1' }, initial_window_pt: '1', reservation: false,
      price_details: { cpu_hourly_pt: '1', disk_hourly_pt: '0', gpu_hourly_pt: '0', memory_hourly_pt: '0', total_hourly_pt: '1' },
    },
  };
}

const password = '  private root password🙂  ';
const publicKey = `ssh-ed25519 ${btoa('\0\0\0\x0bssh-ed25519\0\0\0\x20' + String.fromCharCode(...'d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a'.match(/../gu)!.map((byte) => Number.parseInt(byte, 16))))}`;
const keyText = `${publicKey} private-laptop-comment`;
const draft = () => ({ mode: 'ssh-key' as const, rootPassword: password, rootPasswordConfirmation: password, sshPublicKeyText: keyText });
const passwordDraft = () => ({ ...draft(), mode: 'password' as const });
const login = (mode: 'password' | 'ssh-key' = 'ssh-key') => mode === 'password' ? { root_password: password } : { ssh_public_keys: [publicKey] };
const failure = (status = 0) => new ApiError(status, { code: status === 401 ? 'unauthenticated' : 'validation_failed', details: { ...login() }, message: `错误回显 ${password} ${keyText}` }, 'request-1', null);

beforeEach(() => {
  vi.clearAllMocks();
  runtime.key = 0;
  runtime.auth = reactive({ me: { user: { id: 'user-a', role: 'user' } } });
  runtime.access = reactive({ loginExpired: false });
  setActivePinia(createPinia());
  runtime.quote.mockImplementation(async () => ({ data: selection().quote }));
  runtime.credit.mockImplementation(async () => ({ data: selection().account }));
  runtime.create.mockResolvedValue({ data: { task_id: 'task-a' } });
});

async function prepared() {
  const store = useInstanceOrderStore();
  const memory = useInstanceLoginDraft();
  memory.save(draft());
  expect(await store.prepare(selection())).toBe(true);
  return { store, memory };
}

function enforcement(id: string, status: NonNullable<InstanceOrderSelection['account']>['enforcements'][number]['status']) {
  return {
    id, status, user_id: 'user-a', instance_ids: [`instance-${id}`], policy_version_id: `credit-${id}`,
    revision: 'event-rev-1', exhausted_at: '2026-11-03T00:00:00Z', shutdown_deadline: '2026-11-04T00:00:00Z',
  };
}

describe('shared wallet enforcement guards', () => {
  it.each(['grace', 'stopping', 'unknown', 'completed'] as const)(
    'blocks a %s event even when another policy event is cancelled',
    async (status) => {
      const store = useInstanceOrderStore();
      useInstanceLoginDraft().save(draft());
      const selected = selection();
      selected.account.enforcements = [enforcement('cancelled', 'cancelled'), enforcement('other-policy', status)];
      expect(await store.prepare(selected)).toBe(true);
      expect(await store.submit()).toBeNull();
      expect(runtime.create).not.toHaveBeenCalled();
      expect(runtime.quote).not.toHaveBeenCalled();
    },
  );

  it.each(['grace', 'stopping', 'unknown', 'completed'] as const)(
    'blocks a %s event discovered during final preflight',
    async (status) => {
      const { store } = await prepared();
      const latestAccount = selection().account;
      latestAccount.enforcements = [enforcement('cancelled', 'cancelled'), enforcement('other-policy', status)];
      runtime.credit.mockResolvedValueOnce({ data: latestAccount });
      expect(await store.submit()).toBeNull();
      expect(runtime.create).not.toHaveBeenCalled();
      expect(store.hasPendingIntent).toBe(false);
      expect(store.account?.enforcements).toEqual(latestAccount.enforcements);
    },
  );

  it('allows creation when every policy event is cancelled', async () => {
    const store = useInstanceOrderStore();
    useInstanceLoginDraft().save(draft());
    const selected = selection();
    selected.account.enforcements = [enforcement('policy-a', 'cancelled'), enforcement('policy-b', 'cancelled')];
    runtime.credit.mockResolvedValueOnce({ data: selected.account });
    expect(await store.prepare(selected)).toBe(true);
    expect(await store.submit()).toEqual({ task_id: 'task-a' });
    expect(runtime.create).toHaveBeenCalledTimes(1);
  });
});

describe('private instance credential boundaries', () => {
  it('requires valid credentials for the selected mode before confirmation or creation', async () => {
    const store = useInstanceOrderStore();
    const memory = useInstanceLoginDraft();
    for (const invalid of [emptyLoginDraft(), { ...passwordDraft(), rootPassword: '', rootPasswordConfirmation: '' }, { ...passwordDraft(), rootPasswordConfirmation: '' }, { ...passwordDraft(), rootPassword: 'short', rootPasswordConfirmation: 'short' }, { ...draft(), sshPublicKeyText: '' }, { ...draft(), sshPublicKeyText: 'private-invalid-key' }]) {
      memory.save(invalid);
      expect(await store.prepare(selection())).toBe(false);
      expect(store.order).toBeNull();
      expect(await store.submit()).toBeNull();
    }
    expect(runtime.create).not.toHaveBeenCalled();
  });

  it('does not invalidate or request quotes when private password/key drafts change or clear', async () => {
    const { store, memory } = await prepared();
    const quote = store.quote;
    memory.save({ ...draft(), rootPassword: 'replacement-password', rootPasswordConfirmation: 'replacement-password', sshPublicKeyText: 'edited-public-key' });
    memory.save(emptyLoginDraft());
    expect(store.quote).toBe(quote);
    expect(runtime.quote).not.toHaveBeenCalled();
    await store.refreshQuote();
    for (const [body] of runtime.quote.mock.calls) {
      expect(body).not.toHaveProperty('login');
      expect(JSON.stringify(body)).not.toContain(password);
      expect(JSON.stringify(body)).not.toContain('replacement-password');
      expect(JSON.stringify(body)).not.toContain(publicKey);
      expect(JSON.stringify(body)).not.toContain('edited-public-key');
    }
  });

  it('does not send credentials if authentication expires during final preflight', async () => {
    const { store, memory } = await prepared();
    const quoted = Promise.withResolvers<{ data: InstanceOrderSelection['quote'] }>();
    const started = Promise.withResolvers<void>();
    runtime.quote.mockImplementationOnce(() => { started.resolve(); return quoted.promise; });
    const submitted = store.submit();
    await started.promise;
    runtime.access!.loginExpired = true;
    quoted.resolve({ data: selection().quote });
    expect(await submitted).toBeNull();
    expect(runtime.create).not.toHaveBeenCalled();
    expect(store.hasPendingIntent).toBe(false);
    expect(memory.read()).toEqual(emptyLoginDraft());
    runtime.access!.loginExpired = false;
    expect(memory.read()).toEqual(draft());
  });

  it('keeps the confirmed login immutable when a draft is edited before browser-forward navigation', async () => {
    const { store, memory } = await prepared();
    memory.save({ ...passwordDraft(), rootPassword: 'edited-password', rootPasswordConfirmation: 'edited-password', sshPublicKeyText: '' });
    expect(memory.read().rootPassword).toBe('edited-password');
    expect(memory.read().sshPublicKeyText).toBe('');
    expect(store.loginSummary).toEqual({ mode: 'ssh-key', sshKeyCount: 1 });
    await store.submit();
    expect(runtime.create.mock.calls[0]![0].login).toEqual(login());
  });

  it('keeps credentials out of Pinia state, public forms, order snapshots and every quote', async () => {
    const { store, memory } = await prepared();
    store.saveForm({ name: 'my-instance', gpuCount: 0, gpuCap: '0', resources: selection().spec.resources, rootPassword: password, sshPublicKeyText: keyText } as Parameters<typeof store.saveForm>[0]);
    const injected = selection();
    Object.assign(injected.spec, { login: login() });
    expect(await store.prepare(injected)).toBe(true);
    expect(store.loginSummary).toEqual({ mode: 'ssh-key', sshKeyCount: 1 });
    expect(JSON.stringify(store.$state)).not.toContain(password);
    expect(JSON.stringify(store.$state)).not.toContain(publicKey);
    expect(JSON.stringify(store.$state)).not.toContain('private-laptop-comment');
    expect(store.order?.spec).not.toHaveProperty('login');
    expect(store.form).not.toHaveProperty('rootPassword');
    expect(store.form).not.toHaveProperty('sshPublicKeyText');
    expect(store).not.toHaveProperty('pendingIntent');
    const changed = memory.read();
    changed.rootPassword = 'changed-copy';
    changed.sshPublicKeyText = 'changed-copy';
    expect(memory.read().rootPassword).toBe(password);
    expect(memory.read().sshPublicKeyText).toBe(keyText);
    await store.refreshQuote();
    await store.submit();
    expect(runtime.create).toHaveBeenCalledTimes(1);
    expect(runtime.create.mock.calls[0]![0].login).toEqual(login());
    expect(runtime.quote.mock.calls.length).toBeGreaterThanOrEqual(2);
    for (const [body] of runtime.quote.mock.calls) {
      expect(body).not.toHaveProperty('login');
      expect(body).not.toHaveProperty('name');
      expect(JSON.stringify(body)).not.toContain(password);
      expect(JSON.stringify(body)).not.toContain(publicKey);
      expect(JSON.stringify(body)).not.toContain('private-laptop-comment');
    }
    expect(memory.read()).toEqual(emptyLoginDraft());
    expect(store.loginSummary).toBeNull();
    expect(runtime.track).toHaveBeenCalledWith('task-a', '创建实例 my-instance');
  });

  it.each(['password', 'ssh-key'] as const)('retries the exact private %s body/key after uncertainty and reauthentication', async (mode) => {
    const store = useInstanceOrderStore();
    const memory = useInstanceLoginDraft();
    memory.save({ ...draft(), mode });
    expect(await store.prepare(selection())).toBe(true);
    runtime.create.mockRejectedValueOnce(failure());
    expect(await store.submit()).toBeNull();
    expect(store.hasPendingIntent).toBe(true);
    expect(store.locked).toBe(true);
    expect(JSON.stringify(store.$state)).not.toContain(password);
    expect(JSON.stringify(store.submitError)).not.toContain(password);
    expect(JSON.stringify(store.$state)).not.toContain(publicKey);
    expect(JSON.stringify(store.submitError)).not.toContain(publicKey);
    expect(JSON.stringify(store.submitError)).not.toContain('private-laptop-comment');
    const original = runtime.create.mock.calls[0]!;
    const quotes = runtime.quote.mock.calls.length;
    runtime.access!.loginExpired = true;
    expect(await store.submit()).toBeNull();
    runtime.access!.loginExpired = false;
    memory.save({ ...draft(), mode: mode === 'password' ? 'ssh-key' : 'password', rootPassword: 'replacement', rootPasswordConfirmation: 'replacement', sshPublicKeyText: '' });
    expect(await store.prepare(selection())).toBe(false);
    runtime.create.mockRejectedValueOnce(failure(401));
    expect(await store.submit()).toBeNull();
    expect(store.hasPendingIntent).toBe(true);
    runtime.create.mockResolvedValueOnce({ data: { task_id: 'task-a' } });
    expect(await store.submit()).toEqual({ task_id: 'task-a' });
    expect(runtime.quote).toHaveBeenCalledTimes(quotes);
    for (const [body, options] of runtime.create.mock.calls) {
      expect(body).toBe(original[0]);
      expect(options.key).toBe(original[1].key);
      expect(body.login).toEqual(login(mode));
      if (mode === 'ssh-key') expect(Object.isFrozen(body.login.ssh_public_keys)).toBe(true);
    }
    expect(memory.read()).toEqual(emptyLoginDraft());
    expect(store.hasPendingIntent).toBe(false);
  });

  it('retains a private editable draft after a definitive rejection, never its error echo', async () => {
    const { store, memory } = await prepared();
    runtime.create.mockRejectedValueOnce(failure(422));
    await store.submit();
    expect(store.hasPendingIntent).toBe(false);
    expect(store.locked).toBe(false);
    expect(store.quote).toBeNull();
    expect(memory.read()).toEqual(draft());
    expect(JSON.stringify(store.submitError)).not.toContain(password);
    expect(JSON.stringify(store.submitError)).not.toContain(publicKey);
    expect(JSON.stringify(store.submitError)).not.toContain('private-laptop-comment');
    expect(await store.prepare(selection())).toBe(true);
  });

  it('creates password-only instances while ignoring inactive public-key text', async () => {
    const store = useInstanceOrderStore();
    useInstanceLoginDraft().save(passwordDraft());
    expect(await store.prepare(selection())).toBe(true);
    expect(store.loginSummary).toEqual({ mode: 'password', passwordSet: true });
    await store.submit();
    expect(runtime.create.mock.calls[0]![0].login).toEqual({ root_password: password });
  });

  it('creates key-only instances without a password and freezes only the key-count summary', async () => {
    const store = useInstanceOrderStore();
    useInstanceLoginDraft().save({ ...draft(), rootPassword: '', rootPasswordConfirmation: '' });
    expect(await store.prepare(selection())).toBe(true);
    expect(store.loginSummary).toEqual({ mode: 'ssh-key', sshKeyCount: 1 });
    await store.submit();
    expect(runtime.create.mock.calls[0]![0].login).toEqual({ ssh_public_keys: [publicKey] });
  });

  it('clears all private references on account switch/logout and ignores old responses', async () => {
    const { store, memory } = await prepared();
    const { promise: inFlight, resolve: resolveCreate } = Promise.withResolvers<{ data: { task_id: string } }>();
    const { promise: sent, resolve: signalSent } = Promise.withResolvers<void>();
    runtime.create.mockImplementationOnce(() => { signalSent(); return inFlight; });
    const submitted = store.submit();
    await sent;
    runtime.auth!.me = { user: { id: 'user-b', role: 'user' } };
    expect(memory.read()).toEqual(emptyLoginDraft());
    expect(store.order).toBeNull();
    expect(store.hasPendingIntent).toBe(false);
    resolveCreate({ data: { task_id: 'old-task' } });
    expect(await submitted).toBeNull();
    expect(runtime.track).not.toHaveBeenCalled();
    memory.save(draft());
    runtime.auth!.me = null;
    expect(memory.read()).toEqual(emptyLoginDraft());
  });

  it('does not prepare unsupported node images, while quotation remains credential-free', async () => {
    const store = useInstanceOrderStore();
    useInstanceLoginDraft().save(draft());
    const value = selection();
    value.image.nodes[0]!.cloud_init = false;
    expect(await store.prepare(value)).toBe(false);
    expect(store.order).toBeNull();
    expect(runtime.create).not.toHaveBeenCalled();
  });
});
