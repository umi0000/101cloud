import type { EChartsOption } from 'echarts';
import type { BillingDistribution } from '#/api/types';
import { formatPt } from '#/utils/format';

export interface DistributionSlice {
  id: string;
  label: string;
  consumed: string;
  recorded?: string;
  estimated?: string;
}

export function typeDistribution(data: BillingDistribution): DistributionSlice[] {
  return ([['cpu_pt', 'CPU'], ['memory_pt', '内存'], ['disk_pt', '硬盘'], ['gpu_pt', 'GPU']] as const).map(([key, label]) => ({
    id: key, label, consumed: data.breakdown[key],
  }));
}

export function instanceDistribution(data: BillingDistribution): DistributionSlice[] {
  return data.instances.map((item) => ({
    id: item.instance_id,
    label: `${item.name} #${item.instance_id}${item.deleted ? '（已删除）' : ''}`,
    consumed: item.consumed_pt,
    recorded: item.recorded_pt,
    estimated: item.estimated_pt,
  }));
}

export function distributionTooltip(item: DistributionSlice | undefined, partial: boolean): string {
  if (!item) return '';
  const lines = [item.label, `消耗 ${formatPt(item.consumed)} Pt`];
  if (item.recorded !== undefined) lines.push(`已记录 ${formatPt(item.recorded)} Pt`);
  if (item.estimated !== undefined) lines.push(`未结算尾段估算 ${formatPt(item.estimated)} Pt`);
  if (partial) lines.push('数据不完整，部分消耗待确认');
  return lines.join('\n');
}

export function distributionOption(slices: DistributionSlice[], partial: boolean): EChartsOption {
  const byLabel = new Map(slices.map((item) => [item.label, item]));
  const hasConsumption = slices.some((item) => Number(item.consumed) > 0);
  return {
    animation: false,
    color: ['#3b82f6', '#14b8a6', '#f59e0b', '#a78bfa', '#f472b6', '#22c55e', '#fb7185', '#38bdf8'],
    tooltip: {
      trigger: 'item', renderMode: 'richText', confine: true,
      formatter: (params: unknown) => {
        const index = params && typeof params === 'object' && 'dataIndex' in params && typeof params.dataIndex === 'number' ? params.dataIndex : -1;
        const text = distributionTooltip(slices[index], partial);
        const percent = params && typeof params === 'object' && 'percent' in params ? params.percent : undefined;
        return text && typeof percent === 'number' && Number.isFinite(percent)
          ? `${text}\n${partial ? '已知消耗占比' : '占比'} ${percent.toFixed(2)}%`
          : text;
      },
    },
    title: hasConsumption ? undefined : {
      text: partial ? `已确认金额为 ${formatPt('0')}` : '暂无消耗', left: 100, top: 'middle', textAlign: 'center',
      textStyle: { fontSize: 14, fontWeight: 'normal' },
    },
    legend: {
      type: 'scroll', orient: 'vertical', left: 212, right: 4, top: 'middle', height: '94%',
      selectedMode: false,
      data: slices.map((item) => item.label),
      textStyle: { width: 200, overflow: 'truncate', fontSize: 12 },
      formatter: (name: string) => `${name}\n${formatPt(byLabel.get(name)?.consumed)} Pt`,
      pageTextStyle: { fontSize: 12 },
    },
    series: [{
      type: 'pie', left: 0, width: 200, radius: ['48%', '88%'], center: ['50%', '50%'],
      stillShowZeroSum: false, showEmptyCircle: false, minAngle: 0,
      label: { show: false }, labelLine: { show: false },
      data: slices.map((item) => ({ id: item.id, name: item.label, value: Number(item.consumed) })),
    }],
    media: [{
      query: { maxWidth: 420 },
      option: { title: { left: 68 }, series: [{ width: 136 }], legend: { left: 144, textStyle: { width: 95, fontSize: 11 } } },
    }, {
      option: { title: { left: 100 }, series: [{ width: 200 }], legend: { left: 212, textStyle: { width: 200, fontSize: 12 } } },
    }],
  };
}
