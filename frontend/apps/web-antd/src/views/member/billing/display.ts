import type { BillingOverview, BillingTrend } from '#/api/types';
import { formatPt } from '#/utils/format';

export function forecastText(forecast: BillingOverview['forecast'] | undefined): string {
  if (!forecast || forecast.status === 'unavailable') return '暂不可估算';
  if (forecast.status === 'not_consuming') return '当前不消耗额度';
  if (forecast.status === 'exhausted') return '额度已耗尽';
  if (forecast.seconds === null || !/^\d+$/.test(forecast.seconds)) return '暂不可估算';
  const seconds = BigInt(forecast.seconds);
  if (seconds < 60n) return '不足 1 分钟';
  const minutes = seconds / 60n;
  if (minutes < 60n) return `约 ${minutes} 分钟`;
  const hours = minutes / 60n;
  if (hours < 24n) return `约 ${hours} 小时 ${minutes % 60n} 分钟`;
  return `约 ${hours / 24n} 天 ${hours % 24n} 小时`;
}

export function bucketLabel(trend: BillingTrend, from: string): string {
  return trend.granularity === 'hour' ? `${from.slice(11, 13)}:00`
    : trend.granularity === 'month' ? `${from.slice(5, 7)}月` : from.slice(5, 10);
}

export function bucketTooltip(trend: BillingTrend, index: number): string {
  const item = trend.items[index];
  if (!item) return '';
  const label = bucketLabel(trend, item.from);
  if (item.consumed_pt === null) return `${label} · 尚未发生`;
  return `${label}${item.partial ? ' · 未完整时段' : ''}\n消耗 ${formatPt(item.consumed_pt)} Pt\n已记录 ${formatPt(item.recorded_pt)} Pt\n未结算尾段估算 ${formatPt(item.estimated_pt)} Pt`;
}
