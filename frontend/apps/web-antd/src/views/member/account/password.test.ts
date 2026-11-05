import type { App, Slots } from 'vue';
import { createApp, h, nextTick, reactive } from 'vue';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiError } from '#/api/request';

const runtime = vi.hoisted(() => ({
  auth: { me: { user: { id: '2', role: 'user', username: 'member' } }, clear: vi.fn(), logout: vi.fn() },
  access: { loginExpired: false, setLoginExpired: vi.fn() },
  me: vi.fn(), change: vi.fn(), replace: vi.fn(), push: vi.fn(), success: vi.fn(),
}));
vi.mock('@vben/stores', () => ({ useAccessStore: () => runtime.access }));
vi.mock('@vben/constants', () => ({ LOGIN_PATH: '/auth/login' }));
vi.mock('#/store/auth', () => ({ useAuthStore: () => runtime.auth }));
vi.mock('vue-router', () => ({ onBeforeRouteLeave: vi.fn(), useRouter: () => ({ replace: runtime.replace, push: runtime.push }) }));
vi.mock('#/api/backend', () => ({ authApi: { me: runtime.me, changePassword: runtime.change } }));
vi.mock('@vben/common-ui', () => ({ Page: { setup: (_: unknown, { slots }: { slots: Slots }) => () => h('main', slots.default?.()) } }));
vi.mock('#/components/error-feedback.vue', () => ({ default: { props: ['feedback'], setup: (p: { feedback: { text: string } }) => () => h('p', { role: 'alert' }, p.feedback.text) } }));
vi.mock('ant-design-vue', () => {
  const container = { setup: (_: unknown, { slots }: { slots: Slots }) => () => h('div', [slots.default?.(), slots.description?.(), slots.action?.()]) };
  return {
    Alert: container, Card: container, message: { success: runtime.success },
    Form: { setup: (_: unknown, { slots }: { slots: Slots }) => () => h('form', slots.default?.()) },
    FormItem: { props: ['help'], setup: (p: { help: string }, { slots }: { slots: Slots }) => () => h('div', [slots.default?.(), h('p', p.help)]) },
    Button: { props: ['htmlType', 'disabled'], setup: (p: { htmlType?: string; disabled?: boolean }, { slots }: { slots: Slots }) => () => h('button', { type: p.htmlType ?? 'button', disabled: p.disabled }, slots.default?.()) },
    InputPassword: { props: ['value', 'disabled'], emits: ['update:value'], setup: (p: { value: string; disabled: boolean }, { emit }: { emit: (event: string, value: string) => void }) => () => h('input', { type: 'password', value: p.value, disabled: p.disabled, onInput: (e: Event) => emit('update:value', (e.target as HTMLInputElement).value) }) },
  };
});
import Password from './password.vue';
let app: App; let root: HTMLElement;
async function flush() { for (let n = 0; n < 16; n++) await Promise.resolve(); await nextTick(); }
async function mount() { root = document.createElement('div'); document.body.append(root); app = createApp(Password); app.mount(root); await flush(); }
async function fill(current = 'old-password', next = 'new-password-123', confirm = next) {
  for (const [id, value] of [['member-current-password', current], ['member-new-password', next], ['member-confirm-password', confirm]]) {
    const input = root.querySelector<HTMLInputElement>(`#${id}`)!; input.value = value!; input.dispatchEvent(new Event('input', { bubbles: true }));
  }
  await nextTick();
}
async function submit() { root.querySelector('form')!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true })); await flush(); }
const error = (status: number, code: string) => new ApiError(status, { code, message: '', details: null }, 'test-request', null);
beforeEach(() => {
  vi.clearAllMocks();
  runtime.auth = reactive({ me: { user: { id: '2', role: 'user', username: 'member' } }, clear: vi.fn(), logout: vi.fn() });
  runtime.access = reactive({ loginExpired: false, setLoginExpired: vi.fn() });
  runtime.me.mockResolvedValue({ data: { user: { id: '2', role: 'user' } }, etag: '"4"' });
  runtime.change.mockResolvedValue({ status: 204 });
});
afterEach(() => { app?.unmount(); root?.remove(); });

describe('member password change', () => {
  it('does not submit invalid length or mismatched confirmation, and counts Unicode characters', async () => {
    await mount(); await fill('old', 'short'); await submit(); expect(runtime.change).not.toHaveBeenCalled();
    await fill('old', 'long-password', 'different'); await submit(); expect(runtime.change).not.toHaveBeenCalled();
    await fill('old', '界'.repeat(129)); await submit(); expect(runtime.change).not.toHaveBeenCalled();
    const next = '🔐'.repeat(12); await fill(' old with spaces ', next); await submit();
    expect(runtime.change.mock.calls[0]?.[0]).toEqual({ current_password: ' old with spaces ', new_password: next });
    expect(runtime.change.mock.calls[0]?.[1]).toMatchObject({ ifMatch: '"4"' });
    expect(runtime.auth.clear).toHaveBeenCalledOnce(); expect(runtime.replace).toHaveBeenCalledWith('/auth/login');
  });
  it('keeps the user logged in after invalid current credentials and clears only the rejected current input', async () => {
    runtime.change.mockRejectedValue(error(401, 'invalid_credentials'));
    await mount(); await fill(); await submit();
    expect(runtime.auth.clear).not.toHaveBeenCalled(); expect(runtime.auth.logout).not.toHaveBeenCalled(); expect(runtime.replace).not.toHaveBeenCalled();
    expect(root.querySelector<HTMLInputElement>('#member-current-password')?.value).toBe('');
    expect(root.querySelector<HTMLInputElement>('#member-new-password')?.value).toBe('new-password-123');
    expect(root.querySelector<HTMLInputElement>('#member-current-password')?.getAttribute('aria-invalid')).toBe('true');
  });
  it('blocks duplicate submission while a request is in flight', async () => {
    const pending = Promise.withResolvers<unknown>(); runtime.change.mockReturnValue(pending.promise);
    await mount(); await fill(); await submit(); await submit();
    expect(runtime.change).toHaveBeenCalledOnce();
    expect(root.querySelector<HTMLInputElement>('#member-new-password')?.disabled).toBe(true);
    pending.resolve({ status: 204 }); await flush(); expect(runtime.auth.clear).toHaveBeenCalledOnce();
  });
  it('locks uncertain results and retries exactly the same body, ETag and key', async () => {
    runtime.change.mockRejectedValueOnce(error(503, 'service_unavailable'));
    await mount(); await fill(); await submit(); const first = runtime.change.mock.calls[0];
    expect(root.querySelector<HTMLInputElement>('#member-new-password')?.disabled).toBe(true);
    await submit(); expect(runtime.change.mock.calls[1]).toEqual(first); expect(runtime.auth.clear).toHaveBeenCalledOnce();
  });
  it('requires explicit version refresh after conflict without resubmitting automatically', async () => {
    runtime.change.mockRejectedValueOnce(error(412, 'revision_conflict'));
    await mount(); await fill(); await submit(); await submit();
    expect(runtime.change).toHaveBeenCalledOnce(); expect(runtime.me).toHaveBeenCalledOnce();
    runtime.me.mockResolvedValue({ data: { user: { id: '2', role: 'user' } }, etag: '"9"' });
    [...root.querySelectorAll<HTMLButtonElement>('button')].find(b => b.textContent?.includes('刷新账号版本'))!.click(); await flush();
    await submit(); expect(runtime.change.mock.calls[1]?.[1]).toMatchObject({ ifMatch: '"9"' });
    expect(runtime.change.mock.calls[1]?.[1].key).not.toBe(runtime.change.mock.calls[0]?.[1].key);
  });
  it('cannot submit if the account version is missing or belongs to another account', async () => {
    runtime.me.mockResolvedValue({ data: { user: { id: '3', role: 'user' } }, etag: '"4"' });
    await mount(); await fill(); await submit(); expect(runtime.change).not.toHaveBeenCalled(); expect(root.querySelector('[role="alert"]')).not.toBeNull();
  });
  it('clears credentials on account change and ignores the old account response', async () => {
    const pending = Promise.withResolvers<unknown>(); runtime.change.mockReturnValue(pending.promise);
    await mount(); await fill(); await submit();
    runtime.me.mockResolvedValue({ data: { user: { id: '3', role: 'user' } }, etag: '"5"' });
    runtime.auth.me.user.id = '3'; await flush();
    expect(root.querySelector<HTMLInputElement>('#member-new-password')?.value).toBe('');
    pending.resolve({ status: 204 }); await flush(); expect(runtime.auth.clear).not.toHaveBeenCalled(); expect(runtime.replace).not.toHaveBeenCalled();
  });
  it('does not keep plaintext credentials after session expiry', async () => {
    await mount(); await fill(); runtime.access.loginExpired = true; await flush();
    expect(root.querySelector('input[type="password"]')).toBeNull();
    runtime.access.loginExpired = false; await flush();
    expect(root.querySelector<HTMLInputElement>('#member-new-password')?.value).toBe('');
  });
});
