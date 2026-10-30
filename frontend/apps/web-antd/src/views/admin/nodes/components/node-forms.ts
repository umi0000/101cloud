/**
 * 节点表单的共享规则与换算。规则照抄后端 catalog/nodes.go 的
 * nodeConnectionChanges / nodeCapacityChanges，提前在前端拦截明显错误。
 */
import type { Allocatable, NodeConnectionInput, PolicyVersion, UnmanagedInstance } from '#/api/types';

import { bytesToGib, formatPt, gibToBytes } from '#/utils/format';

/** 后端快照中 incus_uuid 可能为 null（Incus 未报告 volatile.uuid），此时不能纳管。 */
export type UnmanagedView = Omit<UnmanagedInstance, 'incus_uuid'> & { incus_uuid: null | string };

/** HTTPS 源：主机名或 [IPv6]，可带端口，不带路径、查询、用户信息。 */
const ENDPOINT_RE = /^https:\/\/(?:\[[\da-f:.]+\]|[a-z\d.-]+)(?::\d{1,5})?\/?$/i;
/** 32 字节无填充 base64url = 43 字符。 */
const TOKEN_RE = /^[\w-]{42}[AEIMQUYcgkosw048]$/;
const CERT_RE = /^[\da-f]{64}$/;

type Validator = (_rule: unknown, value: unknown) => Promise<void>;
interface FieldRule {
  pattern?: RegExp;
  message?: string;
  required?: boolean;
  validator?: Validator;
}

export const connectionRules: Record<keyof NodeConnectionInput, FieldRule[]> = {
  endpoint: [
    { message: '请填写 Agent 地址', required: true },
    { message: '必须是 HTTPS 源，例如 https://10.0.0.5:8443，不带路径', pattern: ENDPOINT_RE },
  ],
  server_cert_sha256: [
    { message: '请填写证书指纹', required: true },
    { message: '必须是 64 位小写十六进制', pattern: CERT_RE },
  ],
  token: [
    { message: '请填写 Agent 令牌', required: true },
    { message: '令牌应为 43 位无填充 base64url（32 字节）', pattern: TOKEN_RE },
  ],
};

export function emptyConnection(endpoint = '', cert = ''): NodeConnectionInput {
  return { endpoint, server_cert_sha256: cert, token: '' };
}

export function connectionBody(c: NodeConnectionInput): NodeConnectionInput {
  return { endpoint: c.endpoint.trim(), server_cert_sha256: c.server_cert_sha256.trim(), token: c.token.trim() };
}

/** 可分配量表单：GiB 输入，提交时换算为字节字符串。 */
export interface AllocatableForm {
  disk_gib: number | undefined;
  memory_gib: number | undefined;
  resident_disk_gib: number | undefined;
  resident_memory_gib: number | undefined;
  resident_vcpus: number | undefined;
  vcpus: number | undefined;
}

export function emptyAllocatable(): AllocatableForm {
  return {
    disk_gib: undefined,
    memory_gib: undefined,
    resident_disk_gib: undefined,
    resident_memory_gib: undefined,
    resident_vcpus: 0,
    vcpus: undefined,
  };
}

export function allocatableToForm(a: Allocatable): AllocatableForm {
  return {
    disk_gib: bytesToGib(a.disk_bytes),
    memory_gib: bytesToGib(a.memory_bytes),
    resident_disk_gib: bytesToGib(a.resident_disk_bytes),
    resident_memory_gib: bytesToGib(a.resident_memory_bytes),
    resident_vcpus: a.resident_vcpus,
    vcpus: a.vcpus,
  };
}

/** 仅在表单校验通过后调用。 */
export function allocatableFromForm(f: AllocatableForm): Allocatable {
  return {
    disk_bytes: gibToBytes(f.disk_gib ?? 0),
    memory_bytes: gibToBytes(f.memory_gib ?? 0),
    resident_disk_bytes: gibToBytes(f.resident_disk_gib ?? 0),
    resident_memory_bytes: gibToBytes(f.resident_memory_gib ?? 0),
    resident_vcpus: f.resident_vcpus ?? 0,
    vcpus: f.vcpus ?? 0,
  };
}

function gibValid(value: number) {
  try {
    gibToBytes(value);
    return true;
  } catch {
    return false;
  }
}

function numberRule(opts: { integer?: boolean; label: string; max?: () => number | undefined; positive: boolean }): FieldRule[] {
  const validator: Validator = async (_rule, raw) => {
    if (raw === null || raw === undefined || raw === '') throw new Error(`请填写${opts.label}`);
    const value = Number(raw);
    if (!Number.isFinite(value) || value < 0) throw new Error(`${opts.label}不能为负`);
    if (opts.positive && value <= 0) throw new Error(`${opts.label}必须大于 0`);
    if (opts.integer && !Number.isInteger(value)) throw new Error(`${opts.label}必须是整数`);
    if (!opts.integer && !gibValid(value)) throw new Error(`${opts.label}最多 3 位小数`);
    const max = opts.max?.();
    if (max !== null && max !== undefined && value > max) throw new Error(`${opts.label}不能超过对应总量`);
  };
  return [{ required: true, validator }];
}

/** form 为当前表单（getter，以便表单对象整体替换后仍能读到新值）。 */
export function allocatableRules(form: () => AllocatableForm): Record<keyof AllocatableForm, FieldRule[]> {
  return {
    disk_gib: numberRule({ label: '磁盘', positive: true }),
    memory_gib: numberRule({ label: '内存', positive: true }),
    resident_disk_gib: numberRule({ label: '常驻磁盘', max: () => form().disk_gib, positive: false }),
    resident_memory_gib: numberRule({ label: '常驻内存', max: () => form().memory_gib, positive: false }),
    resident_vcpus: numberRule({ integer: true, label: '常驻 vCPU', max: () => form().vcpus, positive: false }),
    vcpus: numberRule({ integer: true, label: 'vCPU', positive: true }),
  };
}

/** gpu-pricing 版本 config：{gpu_model, access_mode, gpu_hourly_pt}。 */
export function priceConfig(p: PolicyVersion) {
  const text = (k: string) => {
    const v = p.config[k];
    return typeof v === 'string' ? v : null;
  };
  return { accessMode: text('access_mode'), hourlyPt: text('gpu_hourly_pt'), model: text('gpu_model') };
}

export function gpuPriceLabel(p: PolicyVersion): string {
  return `${p.code} v${p.version} · ${formatPt(priceConfig(p).hourlyPt)} pt/小时`;
}

/** 已发布且已生效。 */
export function priceEffective(p: PolicyVersion): boolean {
  return p.status === 'active' && !!p.effective_at && Date.parse(p.effective_at) <= Date.now();
}
