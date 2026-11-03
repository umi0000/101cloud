import type { Plan, Resources } from '#/api/types';

import { ptToMicros } from './format';

export type CreationResourceKey = keyof Resources;

export interface CreationResourceBound {
  fixed: boolean;
  max: bigint;
  min: bigint;
  step: bigint;
}

const GIB = 1024n ** 3n;
const MAX_BYTES = (1n << 64n) - 1n;
const MAX_VCPUS = (1n << 32n) - 1n;
const MAX_SAFE_INDEX = BigInt(Number.MAX_SAFE_INTEGER);
const RESOURCE_NAMES: Record<CreationResourceKey, string> = {
  disk_bytes: '磁盘',
  memory_bytes: '内存',
  vcpus: 'CPU',
};

function unsignedInteger(value: string): bigint | null {
  return /^\d+$/.test(value) ? BigInt(value) : null;
}

/** GiB 的十进制输入必须能精确表示整数 bytes，不能截断不足一个字节的部分。 */
export function exactGibToBytes(value: string): null | string {
  const match = /^(\d+)(?:\.(\d+))?$/.exec(value.trim());
  if (!match?.[1]) return null;
  const fraction = (match[2] ?? '').replace(/0+$/, '');
  // 一字节的 GiB 表示最多 30 位小数；更多非零小数不可能对应整数 bytes。
  if (fraction.length > 30) return null;
  const scale = 10n ** BigInt(fraction.length);
  const numerator = (BigInt(match[1]) * scale + BigInt(fraction || '0')) * GIB;
  return numerator % scale === 0n ? (numerator / scale).toString() : null;
}

/** 每个整数 bytes 都有有限的十进制 GiB 表示，不经过 Number 或显示用近似格式。 */
export function bytesToExactGib(value: string): string {
  const bytes = unsignedInteger(value);
  if (bytes === null) return value;
  const whole = bytes / GIB;
  const remainder = bytes % GIB;
  if (remainder === 0n) return whole.toString();
  const fraction = ((remainder * 10n ** 30n) / GIB)
    .toString().padStart(30, '0').replace(/0+$/, '');
  return `${whole}.${fraction}`;
}

export function creationResourceBound(plan: Plan, key: CreationResourceKey): CreationResourceBound | null {
  const override = plan.allowed_overrides[key];
  if (!override) {
    const value = key === 'vcpus'
      ? (Number.isSafeInteger(plan.resources.vcpus) ? BigInt(plan.resources.vcpus) : null)
      : unsignedInteger(plan.resources[key]);
    return value !== null && value > 0n
      ? { fixed: true, max: value, min: value, step: 1n }
      : null;
  }
  const min = unsignedInteger(override.min);
  const max = unsignedInteger(override.max);
  const step = unsignedInteger(override.step);
  if (min === null || max === null || step === null || min <= 0n || max < min || step <= 0n) return null;
  return { fixed: false, max, min, step };
}

/** Slider 只转换步数索引；bytes 本身始终保留为 BigInt。 */
export function resourceSliderMaximum(bound: CreationResourceBound): null | number {
  const steps = (bound.max - bound.min) / bound.step;
  return steps <= MAX_SAFE_INDEX ? Number(steps) : null;
}

export function resourceFromSlider(bound: CreationResourceBound, index: number): bigint | null {
  if (!Number.isSafeInteger(index) || index < 0) return null;
  const value = bound.min + BigInt(index) * bound.step;
  return value <= bound.max ? value : null;
}

export function resourceSliderIndex(bound: CreationResourceBound, value: bigint): null | number {
  if (value < bound.min || value > bound.max || (value - bound.min) % bound.step !== 0n) return null;
  const index = (value - bound.min) / bound.step;
  return index <= MAX_SAFE_INDEX ? Number(index) : null;
}

export function creationResourceError(plan: Plan, resources: Resources): string | null {
  for (const key of ['vcpus', 'memory_bytes', 'disk_bytes'] as const) {
    const name = RESOURCE_NAMES[key];
    const value = key === 'vcpus'
      ? (Number.isSafeInteger(resources.vcpus) ? BigInt(resources.vcpus) : null)
      : unsignedInteger(resources[key]);
    if (value === null || value <= 0n || value > (key === 'vcpus' ? MAX_VCPUS : MAX_BYTES)) {
      return `${name}必须是受支持范围内的正整数${key === 'vcpus' ? '' : '字节数'}`;
    }
    const bound = creationResourceBound(plan, key);
    if (!bound) return `${name}套餐范围无效，请重新选择套餐`;
    if (bound.fixed) {
      if (value !== bound.min) return `${name}为套餐固定配置，不可调整`;
    } else if (value < bound.min || value > bound.max || (value - bound.min) % bound.step !== 0n) {
      return `${name}必须在套餐范围内，按下限起算的步长递增`;
    }
  }
  return null;
}

export function estimateRuntimeMinutes(availablePt: string, hourlyPt: string): bigint | null {
  const available = ptToMicros(availablePt);
  const hourly = ptToMicros(hourlyPt);
  if (available === null || hourly === null || available < 0n || hourly <= 0n) return null;
  return (available * 60n) / hourly;
}

export function formatEstimatedRuntime(availablePt: string, hourlyPt: string): string {
  const available = ptToMicros(availablePt);
  const hourly = ptToMicros(hourlyPt);
  if (available === null || hourly === null || available < 0n || hourly < 0n) return '暂无法估算';
  if (hourly === 0n) return '当前配置不消耗额度';
  const minutes = estimateRuntimeMinutes(availablePt, hourlyPt);
  if (minutes === null) return '暂无法估算';
  return `约${minutes / 60n}小时${minutes % 60n}分`;
}
