import type { App, Slots } from 'vue';
import type { BillingEvents, BillingDistribution, BillingOverview, BillingPeriod } from '#/api/types';
import { createApp, h, nextTick, reactive } from 'vue';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
const runtime = vi.hoisted(() => ({ auth: { me: { user: { id: '2', role: 'user' } } }, access: { loginExpired: false }, overview: vi.fn(), events: vi.fn(), distribution: vi.fn(), trend: vi.fn(), instances: vi.fn(), choices: vi.fn(), poll: undefined as undefined | (() => void) }));
vi.mock('@vben/stores', () => ({ useAccessStore: () => runtime.access }));
vi.mock('#/store/auth', () => ({ useAuthStore: () => runtime.auth }));
vi.mock('#/api/backend', () => ({ memberApi: { billingEvents: runtime.events, billingOverview: runtime.overview, billingDistribution: runtime.distribution, billingTrend: runtime.trend, billingInstances: runtime.instances, billingInstanceOptions: runtime.choices } }));
vi.mock('#/composables/api', () => ({ usePolling: (callback: () => void) => { runtime.poll = callback; } }));
vi.mock('@vben/plugins/echarts', () => ({ EchartsUI: { render: () => h('div') }, useEcharts: () => ({ renderEcharts: vi.fn() }) }));
vi.mock('@vben/common-ui', () => ({ Page: { setup: (_: unknown, { slots }: { slots: Slots }) => () => h('main', slots.default?.()) } }));
vi.mock('#/components/status-tag.vue', () => ({ default: { render: () => h('span') } }));
vi.mock('#/components/time-text.vue', () => ({ default: { props: ['value'], setup: (p: { value: string }) => () => h('time', { datetime: p.value }, p.value) } }));
vi.mock('ant-design-vue', () => {
  const container = { setup: (_: unknown, { slots }: { slots: Slots }) => () => h('div', [slots.default?.(), slots.description?.()]) };
  return { Alert: container, Card: container, Spin: container, Tag: container, Tooltip: container, TypographyText: container, Empty: container,
    Button: { setup: (_: unknown, { slots }: { slots: Slots }) => () => h('button', slots.default?.()) },
    Select: { props: ['value', 'options'], emits: ['update:value', 'change', 'dropdownVisibleChange', 'search'], setup: (p: { value: string; options: { value: string; label: string }[] }, { emit, slots }: { emit: (event: string, value: string | boolean) => void; slots: Slots }) => () => h('div', [
      h('select', { value: p.value, onFocus: () => emit('dropdownVisibleChange', true), onBlur: () => emit('dropdownVisibleChange', false), onChange: (e: Event) => { const value = (e.target as HTMLSelectElement).value; emit('update:value', value); emit('change', value); } }, p.options.map(o => h('option', { value: o.value }, o.label))),
      h('input', { 'aria-label': 'selector search', onInput: (e: Event) => emit('search', (e.target as HTMLInputElement).value) }),
      slots.dropdownRender?.({ menuNode: h('span') }),
    ]) },
    InputSearch: { render: () => h('input') }, Pagination: { props: ['current'], emits: ['change'], setup: (p: { current: number }, { emit }: { emit: (event: string, value: number) => void }) => () => h('button', { 'aria-label': '下一页报告', onClick: () => emit('change', p.current + 1) }, '下一页报告') },
  };
});
import Billing from './index.vue';
let app: App; let root: HTMLElement;
async function flush() { for (let n = 0; n < 20; n++) await Promise.resolve(); await nextTick(); }
async function mount() { root = document.createElement('div'); document.body.append(root); app = createApp(Billing); app.mount(root); await flush(); }
function overview(amount: string): { data: BillingOverview } {
  const summary = { from: '2026-11-09T00:00:00+08:00', to: '2026-11-09T12:00:00+08:00', consumed_pt: amount, recorded_pt: amount, estimated_pt: '0.000000', breakdown: { cpu_pt: amount, memory_pt: '0.000000', disk_pt: '0.000000', gpu_pt: '0.000000' } };
  return { data: {
    as_of: '2026-11-09T12:00:00+08:00', timezone: 'Asia/Shanghai',
    periods: { today: summary, week: summary, month: summary, year: summary },
    balance: { available_pt: '77.000000', reserved_pt: '0.000000', outstanding_debt_pt: '0.000000' },
    current: { status: 'unavailable', hourly_pt: null, reason: 'current_rate_unknown' },
    forecast: { status: 'unavailable', seconds: null, exhausts_at: null, reason: 'current_rate_unknown', basis: 'current_balance_at_current_rate' },
    partial: false, reasons: [],
  } };
}
function distribution(amount = '3.123456', period: BillingPeriod = 'today'): { data: BillingDistribution } {
  return { data: {
    as_of: '2026-11-09T12:00:00+08:00', timezone: 'Asia/Shanghai', period,
    from: '2026-11-09T00:00:00+08:00', to: '2026-11-09T12:00:00+08:00',
    consumed_pt: amount, recorded_pt: amount, estimated_pt: '0.000000',
    breakdown: { cpu_pt: amount, memory_pt: '0.000000', disk_pt: '0.000000', gpu_pt: '0.000000' },
    instances: [{ instance_id: '88', name: 'Distribution only', deleted: false, instance_state: 'running', consumed_pt: amount, recorded_pt: amount, estimated_pt: '0.000000' }],
    partial: false, reasons: [],
  } };
}
function eventReport(name = 'Deleted machine'): { data: BillingEvents } {
  return { data: {
    as_of: '2026-11-09T12:00:00+08:00', timezone: 'Asia/Shanghai', period: 'today',
    from: '2026-11-09T00:00:00+08:00', to: '2026-11-09T12:00:00+08:00', page: 1, page_size: 20, total: 25,
    items: (['credit.grant', 'instance.create', 'instance.start', 'instance.stop', 'instance.delete'] as const).map((event_type, index) => ({
      id: String(index), event_type, occurred_at: '2026-11-09T11:00:00+08:00',
      instance_id: event_type === 'credit.grant' ? null : '44', instance_name: event_type === 'credit.grant' ? null : name,
      grant_source: event_type === 'credit.grant' ? 'daily' : null, amount_pt: event_type === 'credit.grant' ? '11.150000' : null,
      available_before_pt: event_type === 'credit.grant' ? '-1.250000' : null,
      available_after_pt: event_type === 'credit.grant' ? '9.900000' : null,
      hourly_before_pt: null, hourly_after_pt: event_type === 'credit.grant' ? null : '0.000000',
    })),
  } };
}
function selectControl(id: string): HTMLSelectElement {
  return root.querySelector<HTMLSelectElement>(`#${id} select`)!;
}
async function changePeriod(id: string, value: BillingPeriod) {
  const select = selectControl(id); select.value = value; select.dispatchEvent(new Event('change', { bubbles: true })); await flush();
}
beforeEach(() => {
  vi.clearAllMocks();
  runtime.auth = reactive({ me: { user: { id: '2', role: 'user' } } }); runtime.access = reactive({ loginExpired: false });
  runtime.overview.mockResolvedValue(overview('12.123456'));
  runtime.events.mockResolvedValue(eventReport());
  runtime.distribution.mockResolvedValue(distribution());
  runtime.trend.mockResolvedValue({ data: { items: [], granularity: 'hour', partial: false } });
  runtime.instances.mockResolvedValue({ data: { items: [], total: 0, partial: false } });
  runtime.choices.mockResolvedValue({ data: { items: [{ instance_id: '7', name: 'Existing', deleted: false }], total: 21, page: 1, page_size: 20 } });
});
afterEach(() => { app?.unmount(); root?.remove(); vi.useRealTimers(); });
describe('billing dashboard requests', () => {
  it.each([
    ['49.000049', '1.000001', '176400', '2026-11-11T13:00:00+08:00', '49.0', '1.0', '约 2 天 1 小时'],
    ['0.000049', '0.000001', '176400', null, '0.0', '0.0', '约 2 天 1 小时'],
    ['1.049999', '1.000000', '3779', null, '1.0', '1.0', '约 1 小时 2 分钟'],
    ['240000000000000000025.000000', '1.000000', '864000000000000000090000', null, '240000000000000000025.0', '1.0', '约 10000000000000000001 天 1 小时'],
  ] as const)('renders exact API duration for %s Pt without recalculating from one-decimal amounts', async (available, hourly, seconds, exhaustsAt, displayedBalance, displayedRate, duration) => {
    const result = overview('12.149999');
    result.data.balance = { available_pt: available, reserved_pt: '2.000000', outstanding_debt_pt: '3.000000' };
    result.data.current = { status: 'ready', hourly_pt: hourly, reason: null };
    result.data.forecast = { status: 'estimated', seconds, exhausts_at: exhaustsAt, reason: null, basis: 'current_balance_at_current_rate' };
    result.data.partial = true;
    runtime.overview.mockResolvedValue(result);
    await mount();
    expect(root.textContent).toContain('12.1 Pt');
    expect(root.textContent).toContain(`可用余额 ${displayedBalance} Pt`);
    expect(root.textContent).toContain(`${displayedRate} Pt/h`);
    expect(root.textContent).toContain(duration);
    const moment = [...root.querySelectorAll('p')].find(p => p.textContent?.startsWith('静态折算时刻'));
    if (exhaustsAt) expect(moment?.querySelector('time')?.getAttribute('datetime')).toBe(exhaustsAt);
    else expect(moment).toBeUndefined();
  });
  it.each([
    ['not_consuming', '0.000000', null, '当前不消耗额度'],
    ['unavailable', null, '0', '暂不可估算'],
    ['estimated', '1.000000', null, '暂不可估算'],
    ['exhausted', '1.000000', '0', '额度已耗尽'],
  ] as const)('keeps %s duration distinct from zero or infinity and omits unknown moments', async (status, hourly, seconds, duration) => {
    const result = overview('0.000000');
    result.data.current = { status: hourly === null ? 'unavailable' : 'ready', hourly_pt: hourly, reason: null };
    result.data.forecast = { status, seconds, exhausts_at: null, reason: null, basis: 'current_balance_at_current_rate' };
    runtime.overview.mockResolvedValue(result);
    await mount();
    expect(root.textContent).toContain(duration);
    expect(root.textContent).not.toContain('静态折算时刻');
    expect(root.textContent).not.toContain('Infinity');
  });
  it.each(['unavailable', 'not_consuming'] as const)('does not present an irrelevant timestamp as an exhaustion date for %s', async (status) => {
    const result = overview('0.000000');
    result.data.forecast = { status, seconds: null, exhausts_at: '2026-11-11T13:00:00+08:00', reason: null, basis: 'current_balance_at_current_rate' };
    runtime.overview.mockResolvedValue(result);
    await mount();
    expect(root.querySelector('time[datetime="2026-11-11T13:00:00+08:00"]')).toBeNull();
  });
  it('loads account overview independently from scoped reporting and keeps the hero on period changes', async () => {
    await mount();
    expect(runtime.overview).toHaveBeenCalledWith();
    await changePeriod('billing-period', 'year');
    expect(runtime.trend).toHaveBeenLastCalledWith({ period: 'year', instance_id: undefined });
    expect(runtime.instances).toHaveBeenCalledWith(expect.objectContaining({ period: 'year', page: 1, page_size: 20 }));
    expect(runtime.overview).toHaveBeenCalledOnce(); expect(root.textContent).toContain('12.1');
  });
  it('ignores old account responses and clears all data on expiry', async () => {
    const old = Promise.withResolvers<unknown>(); runtime.overview.mockReturnValueOnce(old.promise);
    await mount(); runtime.auth.me.user.id = '3'; await flush();
    old.resolve(overview('999.999999')); await flush();
    expect(root.textContent).not.toContain('1000.0'); expect(root.textContent).toContain('12.1');
    runtime.access.loginExpired = true; await flush(); expect(root.textContent).not.toContain('12.1');
  });
  it('keeps independent successful data when one resource fails and preserves stale data on refresh', async () => {
    runtime.trend.mockRejectedValue(new Error('offline')); await mount();
    expect(root.textContent).toContain('12.1'); expect(root.querySelectorAll('[role="alert"]')).toHaveLength(1);
    runtime.overview.mockRejectedValue(new Error('offline'));
    [...root.querySelectorAll('button')].find(button => button.textContent === '刷新数据')!.click(); await flush();
    expect(root.textContent).toContain('12.1'); expect(root.querySelectorAll('[role="alert"]')).toHaveLength(2);
  });
  it('loads choices lazily and reuses them across opening, period, manual and polling refreshes', async () => {
    await mount();
    expect(runtime.instances).toHaveBeenCalledOnce(); expect(runtime.choices).not.toHaveBeenCalled();
    const selector = selectControl('billing-instance');
    selector.dispatchEvent(new Event('focus')); await flush();
    selector.dispatchEvent(new Event('blur')); selector.dispatchEvent(new Event('focus')); await flush();
    await changePeriod('billing-period', 'year');
    [...root.querySelectorAll('button')].find(button => button.textContent === '刷新数据')!.click(); await flush();
    runtime.poll?.(); await flush();
    expect(runtime.choices).toHaveBeenCalledOnce();
    selector.value = '7'; selector.dispatchEvent(new Event('change', { bubbles: true })); await flush();
    expect(runtime.trend).toHaveBeenLastCalledWith({ period: 'year', instance_id: '7' });
    expect(runtime.overview).toHaveBeenCalledTimes(3);
  });
  it('paginates and searches identities independently and keeps the selected instance across pages', async () => {
    vi.useFakeTimers(); await mount();
    const selector = selectControl('billing-instance');
    selector.dispatchEvent(new Event('focus')); await flush();
    selector.value = '7'; selector.dispatchEvent(new Event('change', { bubbles: true })); await flush();
    runtime.choices.mockResolvedValue({ data: { items: [{ instance_id: '8', name: 'Other', deleted: false }], total: 21 } });
    [...root.querySelectorAll('button')].find(button => button.textContent === '下一页')!.click(); await flush();
    expect(runtime.choices).toHaveBeenLastCalledWith({ search: '', page: 2, page_size: 20 });
    const input = root.querySelector<HTMLInputElement>('#billing-instance [aria-label="selector search"]')!;
    input.value = '%_'; input.dispatchEvent(new Event('input'));
    await vi.advanceTimersByTimeAsync(250); await flush();
    expect(runtime.choices).toHaveBeenLastCalledWith({ search: '%_', page: 1, page_size: 20 });
    expect(runtime.instances).toHaveBeenLastCalledWith(expect.objectContaining({ search: '', instance_id: '7' }));
  });
  it('isolates pending choice responses across identity changes and retries option errors', async () => {
    const pending = Promise.withResolvers<unknown>(); runtime.choices.mockReturnValueOnce(pending.promise);
    await mount(); selectControl('billing-instance').dispatchEvent(new Event('focus')); await flush();
    runtime.auth.me.user.id = '3'; await flush();
    pending.resolve({ data: { items: [{ instance_id: '99', name: 'Private old identity', deleted: false }], total: 1 } }); await flush();
    expect(root.textContent).not.toContain('Private old identity'); expect(runtime.choices).toHaveBeenCalledOnce();
    runtime.choices.mockRejectedValueOnce(new Error('offline'));
    selectControl('billing-instance').dispatchEvent(new Event('focus')); await flush();
    expect(root.textContent).toContain('实例选项加载失败');
    [...root.querySelectorAll('button')].find(button => button.textContent === '重试')!.click(); await flush();
    expect(selectControl('billing-instance').querySelector('option[value="7"]')).not.toBeNull(); expect(root.textContent).not.toContain('实例选项加载失败');
    runtime.access.loginExpired = true; await flush(); expect(root.querySelector('#billing-instance option[value="7"]')).toBeNull();
  });
  it('loads five unique reports and keeps the shared distribution window independent of trend and instance filters', async () => {
    await mount();
    expect(runtime.overview).toHaveBeenCalledOnce(); expect(runtime.distribution).toHaveBeenCalledOnce(); expect(runtime.distribution).toHaveBeenCalledWith({ period: 'today' });
    expect(runtime.trend).toHaveBeenCalledOnce(); expect(runtime.instances).toHaveBeenCalledOnce(); expect(runtime.choices).not.toHaveBeenCalled();
    expect(runtime.events).toHaveBeenCalledOnce();
    await changePeriod('billing-distribution-period', 'month');
    expect(runtime.distribution).toHaveBeenLastCalledWith({ period: 'month' });
    expect(runtime.overview).toHaveBeenCalledOnce(); expect(runtime.trend).toHaveBeenCalledOnce(); expect(runtime.instances).toHaveBeenCalledOnce();
    await changePeriod('billing-period', 'year');
    const selector = selectControl('billing-instance'); selector.dispatchEvent(new Event('focus')); await flush();
    selector.value = '7'; selector.dispatchEvent(new Event('change', { bubbles: true })); await flush();
    expect(runtime.distribution).toHaveBeenCalledTimes(2);
    expect(runtime.trend).toHaveBeenLastCalledWith({ period: 'year', instance_id: '7' });
    expect(root.textContent).toContain('Distribution only #88');
    [...root.querySelectorAll('button')].find(button => button.textContent === '刷新数据')!.click(); await flush();
    runtime.poll?.(); await flush();
    expect(runtime.distribution).toHaveBeenCalledTimes(4);
    expect(runtime.distribution).toHaveBeenLastCalledWith({ period: 'month' });
    expect(runtime.overview).toHaveBeenCalledTimes(3); expect(runtime.choices).toHaveBeenCalledOnce();
    expect(runtime.events).toHaveBeenCalledTimes(3);
  });
  it('clears previous-window pies immediately and ignores out-of-order window responses', async () => {
    await mount(); expect(root.textContent).toContain('3.1');
    const old = Promise.withResolvers<{ data: BillingDistribution }>();
    const current = Promise.withResolvers<{ data: BillingDistribution }>();
    runtime.distribution.mockReturnValueOnce(old.promise).mockReturnValueOnce(current.promise);
    await changePeriod('billing-distribution-period', 'week');
    expect(root.textContent).not.toContain('3.1');
    await changePeriod('billing-distribution-period', 'year');
    current.resolve(distribution('5.222222', 'year')); await flush();
    old.resolve(distribution('999.999999', 'week')); await flush();
    expect(root.textContent).toContain('5.2'); expect(root.textContent).not.toContain('1000.0');
    expect(root.textContent).toContain('12.1');
  });
  it('retains only same-window data on refresh errors, discloses partial data, and isolates identity responses', async () => {
    const partial = distribution(); partial.data.partial = true;
    runtime.distribution.mockResolvedValueOnce(partial);
    await mount(); expect(root.textContent).toContain('分布数据不完整');
    runtime.distribution.mockRejectedValueOnce(new Error('offline'));
    [...root.querySelectorAll('button')].find(button => button.textContent === '刷新数据')!.click(); await flush();
    expect(root.textContent).toContain('3.1'); expect(root.querySelectorAll('[role="alert"]')).toHaveLength(1);
    const pending = Promise.withResolvers<{ data: BillingDistribution }>();
    runtime.distribution.mockReturnValueOnce(pending.promise);
    await changePeriod('billing-distribution-period', 'week');
    expect(root.textContent).not.toContain('3.1');
    runtime.auth.me.user.id = '3'; await flush();
    pending.resolve(distribution('999.999999', 'week')); await flush();
    expect(root.textContent).not.toContain('1000.0');
    runtime.access.loginExpired = true; await flush();
    expect(root.textContent).not.toContain('Distribution only'); expect(root.textContent).not.toContain('3.1');
  });
  it('keeps every zero-cost and duplicate-name identity readable without rendering names as markup', async () => {
    const result = distribution('0.000000');
    result.data.instances = Array.from({ length: 25 }, (_, index) => ({
      instance_id: String(index + 1), name: '<img src=x onerror=alert(1)>', deleted: index === 24,
      instance_state: 'running', consumed_pt: '0.000000', recorded_pt: '0.000000', estimated_pt: '0.000000',
    }));
    result.data.breakdown.cpu_pt = '0.000000';
    runtime.distribution.mockResolvedValue(result);
    await mount();
    const amounts = root.querySelector('[aria-label="实例消耗分布金额"]')!;
    expect(amounts.querySelectorAll('dt')).toHaveLength(25);
    expect(amounts.textContent).toContain('#1'); expect(amounts.textContent).toContain('#25（已删除）');
    expect(amounts.textContent).toContain('0.0 Pt'); expect(root.querySelector('img')).toBeNull();
    expect(root.querySelector('[aria-label="计费类型分布金额"]')?.querySelectorAll('dt')).toHaveLength(4);
  });
  it('does not substitute zero amounts or prior-window data when the selected report fails', async () => {
    await mount();
    runtime.distribution.mockRejectedValueOnce(new Error('offline'));
    await changePeriod('billing-distribution-period', 'month');
    expect(root.textContent).not.toContain('3.1');
    expect(root.querySelector('[aria-label="实例消耗分布金额"]')).toBeNull();
    expect(root.textContent).toContain('12.1');
  });
  it('renders rounded event snapshots, unknowns and retained lifecycle identities', async () => {
    const report = eventReport();
    runtime.events.mockResolvedValue(report);
    await mount();
    const rows = root.querySelector('[aria-label="额度事件记录"]')!;
    expect(rows.querySelectorAll('li')).toHaveLength(5);
    expect(rows.textContent).toContain('前 -1.3 Pt');
    expect(rows.textContent).toContain('后 9.9 Pt');
    expect(rows.textContent).toContain('发放额 11.2 Pt');
    expect(rows.textContent).toContain('前 未知 Pt/h');
    expect(rows.textContent).toContain('后 0.0 Pt/h');
    expect(rows.textContent).toContain('Deleted machine · #44');
  });
  it('resets event pages on independent window changes and discards late window and account responses', async () => {
    await mount();
    root.querySelector('[aria-label="额度事件记录"]')!.parentElement!.parentElement!.querySelector<HTMLButtonElement>('[aria-label="下一页报告"]')!.click();
    await flush();
    expect(runtime.events).toHaveBeenLastCalledWith({ period: 'today', page: 2, page_size: 20 });
    const old = Promise.withResolvers<{ data: BillingEvents }>();
    runtime.events.mockReturnValueOnce(old.promise);
    await changePeriod('billing-events-period', 'week');
    expect(runtime.events).toHaveBeenLastCalledWith({ period: 'week', page: 1, page_size: 20 });
    expect(root.querySelector('[aria-label="额度事件记录"]')).toBeNull();
    await changePeriod('billing-events-period', 'year');
    old.resolve(eventReport('Old private identity')); await flush();
    expect(root.textContent).not.toContain('Old private identity');
    expect(runtime.overview).toHaveBeenCalledOnce(); expect(runtime.distribution).toHaveBeenCalledOnce();
    const privateResponse = Promise.withResolvers<{ data: BillingEvents }>();
    runtime.events.mockReturnValueOnce(privateResponse.promise);
    runtime.poll?.(); await flush();
    runtime.auth.me.user.id = '3'; await flush();
    privateResponse.resolve(eventReport('Other account private')); await flush();
    expect(root.textContent).not.toContain('Other account private');
    runtime.access.loginExpired = true; await flush();
    expect(root.querySelector('[aria-label="额度事件记录"]')).toBeNull();
  });
  it('retains same-filter event snapshots on failure but never shows old-window results as a successful empty response', async () => {
    await mount();
    runtime.events.mockRejectedValueOnce({ code: 'event_source_unavailable', message: '事件服务暂不可用', details: null });
    runtime.poll?.(); await flush();
    expect(root.querySelector('[aria-label="额度事件记录"]')).not.toBeNull();
    expect(root.querySelectorAll('[role="alert"]')).toHaveLength(1);
    expect(root.textContent).toContain('event_source_unavailable');
    expect(root.textContent).toContain('12.1');
    runtime.events.mockRejectedValueOnce({ code: 'event_window_unavailable', message: '所选窗口暂不可用', details: null });
    await changePeriod('billing-events-period', 'month');
    expect(root.querySelector('[aria-label="额度事件记录"]')).toBeNull();
    expect(root.textContent).toContain('event_window_unavailable');
  });
});
