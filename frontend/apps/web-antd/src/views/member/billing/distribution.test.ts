import type { BillingDistribution } from '#/api/types';
import { describe, expect, it } from 'vitest';
import { distributionOption, distributionTooltip, instanceDistribution } from './distribution';

function report(): BillingDistribution {
  return {
    as_of: '2026-11-09T12:00:00+08:00', timezone: 'Asia/Shanghai', period: 'today',
    from: '2026-11-09T00:00:00+08:00', to: '2026-11-09T12:00:00+08:00',
    consumed_pt: '0.000000', recorded_pt: '0.000000', estimated_pt: '0.000000',
    breakdown: { cpu_pt: '0.000000', memory_pt: '0.000000', disk_pt: '0.000000', gpu_pt: '0.000000' },
    instances: [], partial: false, reasons: [],
  };
}

describe('billing distribution presentation', () => {

  it('never truncates or merges the cohort and disambiguates duplicate names using IDs', () => {
    const data = report();
    data.instances = Array.from({ length: 27 }, (_, index) => ({
      instance_id: String(index + 1), name: 'Same name', deleted: index === 26, instance_state: index === 26 ? 'stopped' : 'running',
      consumed_pt: index === 0 ? '0.000001' : '0.000000', recorded_pt: '0.000000', estimated_pt: index === 0 ? '0.000001' : '0.000000',
    }));
    const slices = instanceDistribution(data);
    expect(slices).toHaveLength(27); expect(new Set(slices.map(item => item.label)).size).toBe(27);
    expect(slices[26]?.label).toBe('Same name #27（已删除）');
  });

  it('rounds displayed amounts without changing exact strings used by chart geometry', () => {
    const item = { id: '8', label: '<img src=x> #8', consumed: '9007199254740993.050000', recorded: '9007199254740993.000000', estimated: '0.050000' };
    const text = distributionTooltip(item, false);
    expect(text).toContain(item.label);
    expect(text).toContain('消耗 9007199254740993.1 Pt');
    expect(text).toContain('已记录 9007199254740993.0 Pt');
    expect(text).toContain('未结算尾段估算 0.1 Pt');
    const option = distributionOption([item], false);
    const legend = option.legend as { formatter: (name: string) => string };
    expect(legend.formatter(item.label)).toBe(`${item.label}\n9007199254740993.1 Pt`);
    const series = option.series as Array<{ data: Array<{ value: number }> }>;
    expect(series[0]?.data[0]?.value).toBe(Number(item.consumed));
    expect(item.consumed).toBe('9007199254740993.050000');
    expect(distributionTooltip(undefined, false)).toBe('');
  });
});
