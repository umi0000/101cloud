import type { BillingOverview, BillingTrend } from '#/api/types';
import { describe, expect, it } from 'vitest';
import { bucketLabel, bucketTooltip, forecastText } from './display';

const forecast = (status: BillingOverview['forecast']['status'], seconds: null | string = null): BillingOverview['forecast'] => ({ status, seconds, exhausts_at: null, reason: null, basis: 'current_balance_at_current_rate' });
describe('billing forecast presentation', () => {
  it('does not turn unavailable or idle forecasts into zero or infinity', () => {
    expect(forecastText(undefined)).toBe('暂不可估算');
    expect(forecastText(forecast('unavailable', '0'))).toBe('暂不可估算');
    expect(forecastText(forecast('not_consuming'))).toBe('当前不消耗额度');
    expect(forecastText(forecast('exhausted', '0'))).toBe('额度已耗尽');
  });
  it.each([
    ['0', '不足 1 分钟'],
    ['59', '不足 1 分钟'],
    ['60', '约 1 分钟'],
    ['3599', '约 59 分钟'],
    ['3600', '约 1 小时 0 分钟'],
    ['3660', '约 1 小时 1 分钟'],
    ['86399', '约 23 小时 59 分钟'],
    ['86400', '约 1 天 0 小时'],
    ['176400', '约 2 天 1 小时'],
    ['864000000000000000090000', '约 10000000000000000001 天 1 小时'],
  ])('formats API seconds %s without rounding or calendar truncation', (seconds, expected) => {
    expect(forecastText(forecast('estimated', seconds))).toBe(expected);
  });
  it.each([null, '', 'NaN', '-1', '1.5', '1e3'])('keeps missing or malformed seconds %s unavailable', (seconds) => {
    expect(forecastText(forecast('estimated', seconds))).toBe('暂不可估算');
  });
});
const trend: BillingTrend = {
  as_of: '2026-11-09T01:30:00+08:00', timezone: 'Asia/Shanghai', period: 'today', from: '2026-11-09T00:00:00+08:00', to: '2026-11-10T00:00:00+08:00', instance_id: null, granularity: 'hour', partial: false, reasons: [],
  items: [
    { from: '2026-11-09T00:00:00+08:00', to: '2026-11-09T01:00:00+08:00', consumed_pt: '0.000000', recorded_pt: '0.000000', estimated_pt: '0.000000', partial: false },
    { from: '2026-11-09T01:00:00+08:00', to: '2026-11-09T02:00:00+08:00', consumed_pt: '9007199254740993.123456', recorded_pt: '9007199254740993.000001', estimated_pt: '0.123455', partial: true },
    { from: '2026-11-09T02:00:00+08:00', to: '2026-11-09T03:00:00+08:00', consumed_pt: null, recorded_pt: null, estimated_pt: null, partial: false },
  ],
};
describe('billing trend values', () => {
  it('distinguishes genuine zero, estimated partial buckets, and future missing buckets', () => {
    expect(bucketTooltip(trend, 0)).toContain('消耗 0.0 Pt');
    expect(bucketTooltip(trend, 1)).toContain('未完整时段');
    expect(bucketTooltip(trend, 1)).toContain('已记录 9007199254740993.0 Pt');
    expect(bucketTooltip(trend, 1)).toContain('未结算尾段估算 0.1 Pt');
    expect(bucketTooltip(trend, 2)).toBe('02:00 · 尚未发生');
    expect(bucketTooltip(trend, -1)).toBe('');
  });
  it('labels server calendar buckets without converting to browser timezone', () => {
    expect(bucketLabel(trend, trend.items[0]!.from)).toBe('00:00');
    expect(bucketLabel({ ...trend, granularity: 'day' }, trend.from)).toBe('11-09');
    expect(bucketLabel({ ...trend, granularity: 'month' }, trend.from)).toBe('11月');
  });
});
