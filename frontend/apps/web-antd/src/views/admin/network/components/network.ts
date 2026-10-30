/** 网络页共享：格式校验、IPv4 展开。最终校验以后端为准（catalog/network.go）。 */
import type { NetworkPool } from '#/api/types';

/** 后端 gateway 可为 null（创建时未填网关）。 */
export type NetworkPoolView = Omit<NetworkPool, 'gateway'> & { gateway: null | string };

const V4 = String.raw`(?:(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)\.){3}(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)`;
const V6 = String.raw`[\da-fA-F:]*:[\da-fA-F:.]*`;
export const IPV4_RE = new RegExp(`^${V4}$`);
export const IP_RE = new RegExp(`^(?:${V4}|${V6})$`);
export const CIDR_RE = new RegExp(`^(?:${V4}/(?:3[0-2]|[12]?\\d)|${V6}/(?:12[0-8]|1[01]\\d|[1-9]?\\d))$`);

/** 绑定的 parent_interface / incus_network_name / project_name：1–63 字符，字母数字 _ - .，不能是 . 或 ..。 */
export const NETWORK_NAME_RE = /^(?!\.{1,2}$)[\w.-]{1,63}$/;

export const MAX_ADDRESSES = 1000;

/** 点分十进制 → 32 位无符号整数。 */
function v4ToInt(s: string): number {
  return s.split('.').reduce((acc, part) => acc * 256 + Number(part), 0);
}

/** 展开 IPv4 起止地址（含两端）。超过上限或顺序错误时抛出说明。 */
export function expandIPv4(start: string, end: string): string[] {
  if (!IPV4_RE.test(start) || !IPV4_RE.test(end)) throw new Error('只支持 IPv4 起止地址');
  const a = v4ToInt(start);
  const b = v4ToInt(end);
  if (a > b) throw new Error('起始地址不能大于结束地址');
  if (b - a + 1 > MAX_ADDRESSES) throw new Error(`一次最多 ${MAX_ADDRESSES} 个地址`);
  const out: string[] = [];
  for (let n = a; n <= b; n++) out.push([24, 16, 8, 0].map((shift) => Math.floor(n / 2 ** shift) % 256).join('.'));
  return out;
}

/** 文本框内容 → 地址列表（每行一个，忽略空行与首尾空白）。 */
export function parseAddressLines(text: string): string[] {
  return text
    .split(/\r?\n/)
    .map((s) => s.trim())
    .filter(Boolean);
}
