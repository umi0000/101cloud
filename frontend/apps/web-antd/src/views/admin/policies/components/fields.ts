/**
 * 政策 config 字段定义与校验，对应 backend/internal/billing/policies.go 的 validatePolicy：
 * - pt 字段是十进制字符串（最多 6 位小数，非负）；
 * - 时长/计数是 JSON 整数（int，后端要求 1–31536000，个别字段更严）；
 * - 百分比、权重是 JSON 数字（float64），前端用字符串输入并按 6 位小数整数比较；
 * - 所有必填字段必须显式给出，不能为 null；未知字段后端拒绝。
 */
import type { PolicyKind, PolicyVersion } from '#/api/types';

import { formatPt, PT_PATTERN, ptToMicros } from '#/utils/format';

export type FieldType = 'actions' | 'bool' | 'choice' | 'decimal' | 'int' | 'pt' | 'text';

export interface ChoiceOption {
  hint?: string;
  label: string;
  value: string;
}

export interface PolicyField {
  hint?: string;
  key: string;
  label: string;
  max?: number;
  min?: number;
  options?: ChoiceOption[];
  optional?: boolean;
  placeholder?: string;
  type: FieldType;
  unit?: string;
}

/** 表单状态：按控件类型分开存放，便于 v-model 类型匹配。choice/text/pt/decimal 都存字符串。 */
export interface FormState {
  bools: Record<string, boolean>;
  lists: Record<string, string[]>;
  numbers: Record<string, null | number>;
  texts: Record<string, string>;
}

const SECONDS = 31_536_000;
const DECIMAL_PATTERN = /^\d+(\.\d{1,6})?$/;
const GRANT_TIME_PATTERN = /^([01]\d|2[0-3]):[0-5]\d:[0-5]\d$/;

const seconds = (key: string, label: string, max = SECONDS, hint?: string): PolicyField => ({
  hint,
  key,
  label,
  max,
  min: 1,
  type: 'int',
  unit: '秒',
});
const pct = (key: string, label: string): PolicyField => ({ key, label, max: 100, min: 0, type: 'decimal', unit: '%' });

export const IDLE_ACTIONS: ChoiceOption[] = [
  { label: '释放 GPU', value: 'release_gpu' },
  { label: '停止实例', value: 'stop_instance' },
];

export const ACCESS_MODES: ChoiceOption[] = [
  { label: '独占', value: 'exclusive' },
  { label: '共享', value: 'shared' },
];

export const POLICY_FIELDS: Record<PolicyKind, PolicyField[]> = {
  credit: [
    { key: 'daily_allowance_pt', label: '每日重置额度', type: 'pt', unit: 'pt', hint: '每天重置为此固定额度，上一周期未用的每日额度清零、不累加；兑换及管理员额外发放仍按各自有效期保留。' },
    { key: 'timezone', label: '时区', placeholder: 'Asia/Shanghai', type: 'text', hint: 'IANA 时区名，用于划分每日重置周期' },
    { key: 'grant_time', label: '每日重置时间', placeholder: '00:00:00', type: 'text', hint: '格式 HH:MM:SS，按上面的时区；同一周期只发放一次，消耗后不会重复补满。' },
    seconds('daily_validity_seconds', '每日额度最长有效期', 315_360_000, '可使每日额度提前到期，但最迟在下一次每日重置时到期；增大此值不会跨周期累加。'),
    seconds('billing_window_seconds', '计费区间', 86_400),
    seconds('shutdown_grace_seconds', '停机宽限期', 86_400, '额度耗尽后到停机的时长，1–86400 秒'),
    {
      key: 'grace_charge_mode',
      label: '宽限期计费',
      options: [
        { hint: '额度耗尽后的宽限期内不计费', label: '宽限期免费', value: 'waived' },
        { hint: '宽限期内照常计量，记为待结算区间，不会让额度变成负数', label: '宽限期照常计费', value: 'accrue' },
      ],
      type: 'choice',
    },
    {
      key: 'restore_action',
      label: '恢复额度后',
      options: [
        { hint: '宽限期内补足额度后，取消待执行的停机', label: '恢复额度后取消停机', value: 'cancel_pending_stop' },
        { hint: '补足额度后仍按原停机期限停机', label: '保留原停机期限', value: 'keep_deadline' },
      ],
      type: 'choice',
    },
    {
      key: 'storage_charge_mode',
      label: '磁盘计费',
      hint: '确认停止后 CPU、内存、磁盘、GPU 全部停止计费；GPU 可保持绑定并继续占用容量，不会自动释放。确认再次运行后恢复应计费资源。停止失败或状态未知不会提前免计费。',
      options: [
        { hint: '只在实例运行时对磁盘计费', label: '仅运行时计费', value: 'running_only' },
        { hint: '磁盘不计费', label: '磁盘免费', value: 'free' },
      ],
      type: 'choice',
    },
    {
      key: 'offline_charge_mode',
      label: '节点离线时',
      hint: '节点离线不等于实例已停止；此项只处理尚未确认停止的离线区间，不覆盖确认停机后全部资源免费规则。',
      options: [
        { hint: '节点不在线期间产生的费用暂缓计费', label: '节点离线时暂缓计费', value: 'defer' },
        { hint: '节点离线不影响计费', label: '照常计费', value: 'continue' },
      ],
      type: 'choice',
    },
  ],
  'gpu-idle': [
    { key: 'enabled', label: '启用空闲回收', type: 'bool' },
    seconds('default_timeout_seconds', '默认空闲时长'),
    seconds('min_timeout_seconds', '最短空闲时长'),
    seconds('max_timeout_seconds', '最长空闲时长'),
    { key: 'allowed_actions', label: '允许的动作', options: IDLE_ACTIONS, type: 'actions' },
    seconds('graceful_stop_timeout_seconds', '正常停止超时'),
    { key: 'allow_force_stop', label: '允许强制停止', type: 'bool' },
  ],
  'gpu-pricing': [
    { key: 'gpu_model', label: 'GPU 型号', type: 'text', hint: '可从节点上已发现的型号中选择，也可直接输入' },
    { key: 'access_mode', label: '使用方式', options: ACCESS_MODES, type: 'choice' },
    { key: 'gpu_hourly_pt', label: 'GPU 单价', type: 'pt', unit: 'pt/小时' },
  ],
  'gpu-scheduling': [
    seconds('poll_interval_seconds', '轮询间隔', SECONDS, '自动调度流程的检查间隔，不是节点或实例监控的采集间隔；必须小于指标最大时效'),
    seconds('metrics_max_age_seconds', '指标最大时效', SECONDS, 'GPU 动态负载指标超过此时长即视为过期，不用于选卡或自动调度；不影响 GPU 库存信息的有效性，且必须大于轮询间隔'),
    { key: 'sm_weight', label: 'SM 权重', type: 'decimal', hint: '与显存权重之和必须为 1' },
    { key: 'memory_weight', label: '显存权重', type: 'decimal' },
    pct('source_high_load_pct', '源 GPU 高负载阈值'),
    pct('target_low_load_pct', '目标 GPU 低负载阈值'),
    seconds('evaluation_window_seconds', '评估窗口', SECONDS, '实例持续可安全换卡、源 GPU 持续高负载达到此时长，才考虑自动换卡'),
    { ...pct('min_improvement_pct', '最小改善幅度'), hint: '源 GPU 与目标 GPU 的负载分数差至少达到此值（百分点）' },
    seconds('cooldown_seconds', '冷却时间'),
    seconds('rebindings_window_seconds', '重绑统计窗口', SECONDS, '向前回看这段时间，统计每个实例的自动换卡次数'),
    { key: 'max_rebindings_per_window', label: '窗口内最多重绑次数', hint: '单个实例在上述统计窗口内允许的自动换卡次数上限', max: SECONDS, min: 1, type: 'int', unit: '次' },
    { key: 'max_concurrent_per_node', label: '每节点最多并发', hint: '同一节点同时允许的自动换卡任务数，含排队和待核对任务', max: SECONDS, min: 1, type: 'int' },
    { key: 'allow_price_increase', label: '允许调度到更贵的 GPU', type: 'bool' },
  ],
  metrics: [
    { key: 'raw_retention_days', label: '原始数据保留', max: SECONDS, min: 1, type: 'int', unit: '天' },
    { key: 'rollup_retention_days', label: '汇总数据保留', max: SECONDS, min: 1, type: 'int', unit: '天' },
    seconds('bucket_seconds', '汇总粒度'),
    seconds('collection_interval_seconds', '采集间隔'),
    { ...seconds('collection_timeout_seconds', '采集超时', 120), hint: '可选，1–120 秒；留空使用后端默认', optional: true },
    {
      hint: '可选，1–32；留空使用后端默认',
      key: 'max_concurrent_nodes',
      label: '最多并发节点',
      max: 32,
      min: 1,
      optional: true,
      type: 'int',
    },
  ],
  pricing: [
    { key: 'cpu_hourly_pt', label: 'CPU 单价', type: 'pt', unit: 'pt/核·小时' },
    { key: 'memory_gib_hourly_pt', label: '内存单价', type: 'pt', unit: 'pt/GiB·小时' },
    { key: 'disk_gib_hourly_pt', label: '磁盘单价', type: 'pt', unit: 'pt/GiB·小时' },
  ],
};

/** 列表中的关键配置摘要字段。 */
const SUMMARY_KEYS: Record<PolicyKind, string[]> = {
  credit: ['daily_allowance_pt', 'shutdown_grace_seconds'],
  'gpu-idle': ['enabled', 'default_timeout_seconds', 'allowed_actions'],
  'gpu-pricing': ['gpu_model', 'access_mode', 'gpu_hourly_pt'],
  'gpu-scheduling': ['source_high_load_pct', 'target_low_load_pct', 'poll_interval_seconds'],
  metrics: ['raw_retention_days', 'rollup_retention_days', 'bucket_seconds'],
  pricing: ['cpu_hourly_pt', 'memory_gib_hourly_pt', 'disk_gib_hourly_pt'],
};

/** 按字段含义格式化显示（详情、摘要）。缺失显示 "—"。 */
export function displayValue(field: PolicyField, value: unknown): string {
  if (value === undefined || value === null || value === '') return '—';
  switch (field.type) {
    case 'actions': {
      if (!Array.isArray(value)) return String(value);
      return value.map((v) => field.options?.find((o) => o.value === v)?.label ?? String(v)).join('、');
    }
    case 'bool': {
      return value === true ? '是' : value === false ? '否' : String(value);
    }
    case 'choice': {
      return field.options?.find((o) => o.value === value)?.label ?? String(value);
    }
    case 'pt': {
      return `${formatPt(String(value))} ${field.unit ?? 'pt'}`;
    }
    default: {
      return field.unit ? `${String(value)} ${field.unit}` : String(value);
    }
  }
}

/** 原样显示（未知选项、GPU 型号等）。 */
export function rawValue(value: unknown): string {
  if (value === undefined || value === null) return '—';
  if (typeof value === 'string') return value;
  return JSON.stringify(value);
}

export function configSummary(kind: PolicyKind, config: Record<string, unknown> | null | undefined): string {
  if (!config) return '—';
  const fields = POLICY_FIELDS[kind];
  return SUMMARY_KEYS[kind]
    .flatMap((key) => {
      const field = fields.find((f) => f.key === key);
      return field ? [`${field.label} ${displayValue(field, config[key])}`] : [];
    })
    .join('；');
}

/** 版本显示名：`code v{version}`；GPU 定价附型号与使用方式。 */
export function versionLabel(v: PolicyVersion): string {
  const base = `${v.code} v${v.version}`;
  if (v.kind !== 'gpu-pricing') return base;
  const access = ACCESS_MODES.find((o) => o.value === v.config.access_mode)?.label ?? rawValue(v.config.access_mode);
  return `${base}（${rawValue(v.config.gpu_model)} · ${access}）`;
}

export function emptyState(kind: PolicyKind): FormState {
  const s: FormState = { bools: {}, lists: {}, numbers: {}, texts: {} };
  for (const f of POLICY_FIELDS[kind]) {
    if (f.type === 'bool') s.bools[f.key] = false;
    else if (f.type === 'actions') s.lists[f.key] = [];
    else if (f.type === 'int') s.numbers[f.key] = null;
    else s.texts[f.key] = '';
  }
  return s;
}

/** 由已有 config 预填表单。缺失字段保持为空，不补默认值。 */
export function stateFromConfig(kind: PolicyKind, config: Record<string, unknown>): FormState {
  const s = emptyState(kind);
  for (const f of POLICY_FIELDS[kind]) {
    const v = config[f.key];
    if (f.type === 'bool') s.bools[f.key] = v === true;
    else if (f.type === 'actions') s.lists[f.key] = Array.isArray(v) ? v.map(String) : [];
    else if (f.type === 'int') s.numbers[f.key] = typeof v === 'number' ? v : null;
    else s.texts[f.key] = typeof v === 'string' || typeof v === 'number' ? String(v) : '';
  }
  return s;
}

const micros = (s: string) => ptToMicros(s) ?? 0n;

function fieldError(field: PolicyField, s: FormState): null | string {
  switch (field.type) {
    case 'actions': {
      const list = s.lists[field.key] ?? [];
      return list.length === 0 ? '至少选择一项' : null;
    }
    case 'bool': {
      return null;
    }
    case 'choice': {
      const value = s.texts[field.key];
      if (!value) return '请选择';
      return field.options?.some((option) => option.value === value) ? null : '请选择有效选项';
    }
    case 'decimal': {
      const v = (s.texts[field.key] ?? '').trim();
      if (!v) return '必填';
      if (!DECIMAL_PATTERN.test(v)) return '非负数，最多 6 位小数';
      if (field.max !== undefined && micros(v) > BigInt(field.max) * 1_000_000n) return `不能大于 ${field.max}`;
      return null;
    }
    case 'int': {
      const v = s.numbers[field.key];
      if (v === null || v === undefined) return field.optional ? null : '必填';
      if (!Number.isInteger(v)) return '必须是整数';
      if ((field.min !== undefined && v < field.min) || (field.max !== undefined && v > field.max)) {
        return `范围 ${field.min}–${field.max}`;
      }
      return null;
    }
    case 'pt': {
      const v = (s.texts[field.key] ?? '').trim();
      if (!v) return '必填';
      return PT_PATTERN.test(v) ? null : '非负 pt，最多 6 位小数';
    }
    case 'text': {
      const v = s.texts[field.key] ?? '';
      if (!v.trim()) return '必填';
      if (field.key === 'grant_time' && !GRANT_TIME_PATTERN.test(v)) return '格式 HH:MM:SS';
      if (field.key === 'timezone' && !validTimezone(v)) return '不是有效的 IANA 时区';
      if (field.key === 'gpu_model' && v.length > 255) return '最多 255 个字符';
      return null;
    }
  }
}

function validTimezone(tz: string) {
  try {
    return !!new Intl.DateTimeFormat('en-US', { timeZone: tz }).resolvedOptions().timeZone;
  } catch {
    return false;
  }
}

function crossErrors(kind: PolicyKind, s: FormState, errors: Record<string, string>): string[] {
  const out: string[] = [];
  const ok = (...keys: string[]) => keys.every((k) => !errors[k]);
  const n = (k: string) => s.numbers[k] ?? 0;
  const t = (k: string) => micros(s.texts[k] ?? '');
  if (kind === 'gpu-scheduling') {
    if (ok('sm_weight', 'memory_weight') && t('sm_weight') + t('memory_weight') !== 1_000_000n) {
      out.push('SM 权重与显存权重之和必须等于 1');
    }
    if (ok('source_high_load_pct', 'target_low_load_pct') && t('target_low_load_pct') >= t('source_high_load_pct')) {
      out.push('目标 GPU 低负载阈值必须小于源 GPU 高负载阈值');
    }
  } else if (kind === 'gpu-idle') {
    if (ok('min_timeout_seconds', 'default_timeout_seconds', 'max_timeout_seconds')) {
      if (n('min_timeout_seconds') > n('default_timeout_seconds') || n('default_timeout_seconds') > n('max_timeout_seconds')) {
        out.push('需满足 最短空闲时长 ≤ 默认空闲时长 ≤ 最长空闲时长');
      }
    }
  } else if (kind === 'metrics') {
    if (ok('raw_retention_days', 'rollup_retention_days') && n('rollup_retention_days') < n('raw_retention_days')) {
      out.push('汇总数据保留天数不能少于原始数据保留天数');
    }
    if (ok('bucket_seconds', 'collection_interval_seconds') && n('bucket_seconds') < n('collection_interval_seconds')) {
      out.push('汇总粒度不能小于采集间隔');
    }
  }
  return out;
}

export function validateState(kind: PolicyKind, s: FormState): { cross: string[]; fields: Record<string, string> } {
  const fields: Record<string, string> = {};
  for (const f of POLICY_FIELDS[kind]) {
    const e = fieldError(f, s);
    if (e) fields[f.key] = e;
  }
  return { cross: crossErrors(kind, s, fields), fields };
}

/** 生成提交的 config：pt/text/choice 为字符串，decimal 为 JSON 数字，可选整数留空时不提交。 */
export function configFromState(kind: PolicyKind, s: FormState): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const f of POLICY_FIELDS[kind]) {
    switch (f.type) {
      case 'actions': {
        out[f.key] = [...(s.lists[f.key] ?? [])];
        break;
      }
      case 'bool': {
        out[f.key] = s.bools[f.key] === true;
        break;
      }
      case 'decimal': {
        out[f.key] = Number((s.texts[f.key] ?? '').trim());
        break;
      }
      case 'int': {
        const v = s.numbers[f.key];
        if (v !== null && v !== undefined) out[f.key] = v;
        break;
      }
      case 'pt': {
        out[f.key] = (s.texts[f.key] ?? '').trim();
        break;
      }
      default: {
        out[f.key] = s.texts[f.key] ?? '';
      }
    }
  }
  return out;
}
