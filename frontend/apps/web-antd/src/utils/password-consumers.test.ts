import type { User } from '#/api/types';
import type { App, Slots } from 'vue';
import { createApp, h, nextTick } from 'vue';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

const runtime = vi.hoisted(() => ({ register: vi.fn(), reset: vi.fn(), replace: vi.fn(), run: vi.fn(), resetKey: vi.fn() }));
vi.mock('vue-router', () => ({ useRouter: () => ({ replace: runtime.replace }) }));
vi.mock('#/api/backend', () => ({ authApi: { registration: async () => ({ data: { enabled: true } }), register: runtime.register }, usersApi: { resetPassword: runtime.reset } }));
vi.mock('#/store', () => ({ useAuthStore: () => ({ me: { user: { id: '1' } } }) }));
vi.mock('#/composables/api', () => ({ useAction: () => ({ pending: { value: false }, resetKey: runtime.resetKey, run: runtime.run }) }));
vi.mock('#/components/confirm-action.vue', () => ({ default: { props: ['open', 'onConfirm'], setup: (p: { open: boolean; onConfirm: () => void }) => () => p.open ? h('button', { 'data-confirm': '', onClick: p.onConfirm }, 'confirm') : null } }));
vi.mock('#/views/admin/users/components/password-reveal.vue', () => ({ default: { render: () => null } }));
vi.mock('ant-design-vue', () => {
  const container = { setup: (_: unknown, { slots }: { slots: Slots }) => () => h('div', slots.default?.()) };
  const input = { props: ['value', 'disabled'], emits: ['update:value'], setup: (p: { value: string; disabled: boolean }, { emit }: { emit: (event: string, value: string) => void }) => () => h('input', { value: p.value, disabled: p.disabled, onInput: (e: Event) => emit('update:value', (e.target as HTMLInputElement).value) }) };
  return {
    Alert: container, Skeleton: container, Space: { Compact: container },
    Form: { setup: (_: unknown, { slots }: { slots: Slots }) => () => h('form', slots.default?.()) },
    FormItem: { props: ['help'], setup: (p: { help: string }, { slots }: { slots: Slots }) => () => h('div', [slots.default?.(), h('p', p.help)]) },
    Input: input, InputPassword: input,
    Button: { props: ['htmlType', 'disabled'], setup: (p: { htmlType?: string; disabled?: boolean }, { slots }: { slots: Slots }) => () => h('button', { type: p.htmlType ?? 'button', disabled: p.disabled }, slots.default?.()) },
  };
});

import Register from '../views/_core/authentication/register.vue';
import PasswordTab from '../views/admin/users/components/password-tab.vue';

let app: App; let root: HTMLElement;
async function flush() { for (let i = 0; i < 16; i++) await Promise.resolve(); await nextTick(); }
async function fill(selector: string, value: string) { const input = root.querySelector<HTMLInputElement>(selector)!; input.value = value; input.dispatchEvent(new Event('input', { bubbles: true })); await nextTick(); }
function button(text: string) { return [...root.querySelectorAll('button')].find((b) => b.textContent?.includes(text))!; }
beforeEach(() => {
  vi.clearAllMocks(); root = document.createElement('div'); document.body.append(root);
  runtime.register.mockResolvedValue({ data: { username: 'member' } });
  runtime.reset.mockResolvedValue({ status: 204 });
  runtime.run.mockImplementation((fn: (key: string) => unknown) => fn('test-key'));
});
afterEach(() => { app?.unmount(); root.remove(); });

describe('account password form consumers', () => {
  it('registration blocks weak passwords and sends an eight-codepoint mixed password unchanged', async () => {
    app = createApp(Register); app.mount(root); await flush();
    await fill('#register-username', 'member');
    for (const value of ['abcdefgh', 'ABCDEFG1', 'abcdefg1', 'Abcdefgh', 'Ａbcdefg1']) {
      await fill('#register-password', value); await fill('#register-confirm', value);
      root.querySelector('form')!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true })); await flush();
      expect(runtime.register).not.toHaveBeenCalled();
    }
    const value = ' Ab1🙂🙂🙂 ';
    await fill('#register-password', value); await fill('#register-confirm', value);
    root.querySelector('form')!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true })); await flush();
    expect(runtime.register).toHaveBeenCalledWith({ username: 'member', email: null, password: value });
  });

  it('admin reset disables weak passwords and generated passwords satisfy all categories', async () => {
    app = createApp(PasswordTab, { etag: '"4"', user: { id: '2', username: 'target', status: 'active' } as User }); app.mount(root); await flush();
    await fill('input', 'abcdefgh'); expect(button('重置密码').disabled).toBe(true);
    await fill('input', 'Abcdefg1'); expect(button('重置密码').disabled).toBe(false);
    button('重置密码').click(); await nextTick(); root.querySelector<HTMLButtonElement>('[data-confirm]')!.click(); await flush();
    expect(runtime.reset).toHaveBeenCalledWith('2', { new_password: 'Abcdefg1' }, { ifMatch: '"4"', key: 'test-key' });
    button('生成随机密码').click(); await nextTick();
    const generated = root.querySelector<HTMLInputElement>('input')!.value;
    expect(generated).toHaveLength(24); expect(generated).toMatch(/[A-Z]/u); expect(generated).toMatch(/[a-z]/u); expect(generated).toMatch(/[0-9]/u);
    expect(button('重置密码').disabled).toBe(false);
  });
});
