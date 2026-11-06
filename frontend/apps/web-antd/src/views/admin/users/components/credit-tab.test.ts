import { createApp, h, ref } from 'vue';
import { describe, expect, it, vi } from 'vitest';

import CreditTab from './credit-tab.vue';

const api = vi.hoisted(() => ({ createAccount: vi.fn() }));
vi.mock('#/api/backend', () => ({
  financeApi: {
    accounts: async () => ({ data: { items: [] } }),
    createAccount: api.createAccount,
  },
  policiesApi: { list: vi.fn() },
  listAll: async () => [{ id: '17', code: 'credit', version: '1', status: 'active', effective_at: '2020-01-01T00:00:00Z' }],
}));
vi.mock('#/composables/api', () => {
  return {
    errorToast: vi.fn(),
    useAction: () => ({ pending: ref(false), resetKey: vi.fn(), run: (fn: (key: string) => unknown) => fn('test-intent') }),
  };
});

describe('user wallet creation form', () => {
  it('blocks an unselected policy, then submits from the actual form and displays the created account', async () => {
    api.createAccount.mockResolvedValue({ data: {
      id: '91', policy_version_id: '17', status: 'active', available_pt: '0', remaining_pt: '0',
      reserved_pt: '0', outstanding_debt_pt: '0', enforcements: [], next_grant_at: null,
    } });
    const host = document.createElement('div');
    document.body.append(host);
    const app = createApp({ render: () => h(CreditTab, { userId: '1' }) });
    app.component('RouterLink', { render: () => null });
    app.mount(host);
    try {
      await vi.waitFor(() => expect(host.querySelector('button[type=submit]')).not.toBeNull());
      expect((host.querySelector('button[type=submit]') as HTMLButtonElement).disabled).toBe(true);
      host.querySelector('form')!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
      await vi.waitFor(() => expect(host.textContent).toContain('请选择已生效的额度政策'));
      expect(api.createAccount).not.toHaveBeenCalled();
      host.querySelector('.ant-select-selector')!.dispatchEvent(new MouseEvent('mousedown', { bubbles: true }));
      await vi.waitFor(() => expect(document.querySelector('.ant-select-item-option')).not.toBeNull());
      (document.querySelector('.ant-select-item-option') as HTMLElement).click();
      await vi.waitFor(() => expect((host.querySelector('button[type=submit]') as HTMLButtonElement).disabled).toBe(false));
      host.querySelector('form')!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
      await vi.waitFor(() => expect(host.textContent).toContain('账户 ID'));
      expect(api.createAccount).toHaveBeenCalledTimes(1);
      expect(api.createAccount.mock.calls[0]![0]).toEqual({ user_id: '1', policy_version_id: '17' });
      expect(host.textContent).not.toContain('该用户尚未配置共享钱包');
    } finally {
      app.unmount();
      host.remove();
    }
  });
});
