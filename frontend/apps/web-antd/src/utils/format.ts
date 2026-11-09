/**
 * 显示格式化。规则见 frontend/前端设计.md 2.3–2.4：
 * - pt 仅显示为 1 位小数，十进制四舍五入（中点远离零），不做浮点运算。
 * - 字节是十进制字符串，用 BigInt 换算。
 * - 时间按后端 +08:00 原样显示，不按浏览器时区转换。
 */

const GIB = 1024n ** 3n;
const MIB = 1024n ** 2n;

/** 仅用于显示："5.850000" → "5.9"，"-0.050000" → "-0.1"，零统一为 "0.0"。 */
export function formatPt(value: null | string | undefined): string {
  if (value === null || value === undefined || value === '') return '—';
  const match = /^(-?)(\d+)(?:\.(\d+))?$/.exec(value);
  const whole = match?.[2];
  if (!match || whole === undefined) return value;
  const fraction = match[3] ?? '';
  let tenths = BigInt(whole) * 10n + BigInt(fraction[0] ?? '0');
  if ((fraction[1] ?? '0') >= '5') tenths += 1n;
  const digits = tenths.toString().padStart(2, '0');
  const sign = match[1] && tenths !== 0n ? '-' : '';
  return `${sign}${digits.slice(0, -1)}.${digits.slice(-1)}`;
}

/** pt 字符串 → 放大 1e6 的 BigInt，用于比较大小。非法输入返回 null。 */
export function ptToMicros(value: string): bigint | null {
  const m = /^(-?)(\d+)(?:\.(\d{1,6}))?$/.exec(value.trim());
  const whole = m?.[2];
  if (!m || whole === undefined) return null;
  const micros = BigInt(whole) * 1_000_000n + BigInt((m[3] ?? '').padEnd(6, '0') || '0');
  return m[1] ? -micros : micros;
}

export const PT_PATTERN = /^\d+(\.\d{1,6})?$/;
export const SIGNED_PT_PATTERN = /^-?\d+(\.\d{1,6})?$/;

function divide(value: bigint, unit: bigint): string {
  const whole = value / unit;
  const rest = value % unit;
  if (rest === 0n) return whole.toString();
  const hundredths = (rest * 100n) / unit;
  return `≈${whole}.${hundredths.toString().padStart(2, '0').replace(/0$/, '')}`;
}

/** 字节字符串 → "16 GiB" / "512 MiB"。 */
export function formatBytes(value: null | string | undefined): string {
  if (value === null || value === undefined || value === '') return '—';
  let n: bigint;
  try {
    n = BigInt(value);
  } catch {
    return value;
  }
  if (n >= GIB) return `${divide(n, GIB)} GiB`;
  if (n >= MIB) return `${divide(n, MIB)} MiB`;
  return `${n} B`;
}

/** GiB 数字输入 → 字节字符串。 */
export function gibToBytes(gib: number | string): string {
  const text = String(gib).trim();
  const m = /^(\d+)(?:\.(\d{1,3}))?$/.exec(text);
  const whole = m?.[1];
  if (!m || whole === undefined) throw new Error('容量必须是非负数字');
  const frac = (m[2] ?? '').padEnd(3, '0');
  return ((BigInt(whole) * 1000n + BigInt(frac || '0')) * GIB / 1000n).toString();
}

/** 字节字符串 → GiB 数字（仅用于表单回填，整数 GiB 精确）。 */
export function bytesToGib(value: null | string | undefined): number {
  if (!value) return 0;
  const n = BigInt(value);
  return Number((n * 1000n) / GIB) / 1000;
}

export function formatMib(value: null | number | undefined): string {
  if (value === null || value === undefined) return '—';
  return value >= 1024 ? `${Math.round((value / 1024) * 10) / 10} GiB` : `${value} MiB`;
}

export function formatPercent(value: null | number | string | undefined): string {
  if (value === null || value === undefined || value === '') return '未知';
  const n = Number(value);
  if (!Number.isFinite(n)) return '未知';
  return `${Math.round(n * 10) / 10}%`;
}

const TIME_RE = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})/;

/** 后端时间按原样显示：同日 HH:mm:ss，同年 MM-DD HH:mm，否则 YYYY-MM-DD HH:mm。 */
export function formatTime(value: null | string | undefined, now = new Date()): string {
  if (!value) return '—';
  const m = TIME_RE.exec(value);
  if (!m) return value;
  const [, y, mo, d, h, mi, s] = m;
  const local = shanghaiParts(now);
  if (y === local.y && mo === local.mo && d === local.d) return `${h}:${mi}:${s}`;
  if (y === local.y) return `${mo}-${d} ${h}:${mi}`;
  return `${y}-${mo}-${d} ${h}:${mi}`;
}

export function formatFullTime(value: null | string | undefined): string {
  if (!value) return '—';
  const m = TIME_RE.exec(value);
  return m ? `${m[1]}-${m[2]}-${m[3]} ${m[4]}:${m[5]}:${m[6]} +08:00` : value;
}

function shanghaiParts(date: Date) {
  const shifted = new Date(date.getTime() + 8 * 3600_000);
  return {
    d: String(shifted.getUTCDate()).padStart(2, '0'),
    mo: String(shifted.getUTCMonth() + 1).padStart(2, '0'),
    y: String(shifted.getUTCFullYear()),
  };
}

/** Date → 带 +08:00 的 RFC3339，用于请求中的时间字段。 */
export function toShanghaiISO(date: Date): string {
  const s = new Date(date.getTime() + 8 * 3600_000).toISOString().slice(0, 19);
  return `${s}+08:00`;
}

/** 距今多少秒（基于后端时间与本机时钟偏差校准）。 */
export function secondsSince(value: null | string | undefined, skewMs = 0): null | number {
  if (!value) return null;
  const t = Date.parse(value);
  if (Number.isNaN(t)) return null;
  return Math.max(0, Math.round((Date.now() + skewMs - t) / 1000));
}

export function formatAge(seconds: null | number): string {
  if (seconds === null) return '未知';
  if (seconds < 60) return `${seconds} 秒前`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)} 分钟前`;
  if (seconds < 86_400) return `${Math.floor(seconds / 3600)} 小时前`;
  return `${Math.floor(seconds / 86_400)} 天前`;
}

export function shortId(value: null | string | undefined, head = 8, tail = 4): string {
  if (!value) return '—';
  return value.length > head + tail + 1 ? `${value.slice(0, head)}…${value.slice(-tail)}` : value;
}
