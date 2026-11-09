/** 后端错误码中文提示，见 frontend/前端设计.md 第 12 节。 */
import type { PublicError } from '#/api/request';

import { z } from 'zod';

import { ApiError, PublicErrorSchema } from '#/api/request';
import { TaskErrorSourceSchema } from '#/api/task-schema';
import { formatBytes, formatPt } from '#/utils/format';

const MESSAGES: Record<string, string> = {
  action_not_allowed: '管理员未授权此实例执行该操作',
  agent_invalid_response: '节点返回异常',
  capacity_in_use: '资源仍在使用，不能修改',
  configuration_conflict: '实际配置与记录不一致',
  conflict: '当前配置或资源状态存在冲突，请刷新后检查',
  console_limit: '同时打开的控制台已达上限',
  console_not_ready: '控制台会话不可用，请重新连接',
  console_process_mismatch: '控制台会话不可用，请重新连接',
  console_revoked: '控制台权限已失效',
  console_unsupported: '该实例不支持此控制台类型',
  console_used: '控制台会话已被使用，请重新连接',
  credit_exhausted: '可用额度不足',
  csrf_failed: '页面已过期，请刷新后重试',
  external_result_unknown:
    '外部操作结果尚未确认，请联系管理员核对；不会自动重新执行',
  force_not_allowed: '强制操作仅管理员可用',
  forbidden: '没有权限执行该操作',
  gpu_capacity_unavailable: '当前没有符合条件的 GPU',
  gpu_in_use: 'GPU 仍有进程或显存占用',
  gpu_policy_not_authorized: '管理员未授权修改该 GPU 策略项',
  idempotency_conflict: '重复提交的内容不一致',
  idle_action_not_allowed: '该空闲动作不被允许',
  idle_policy_disabled: '平台未启用空闲回收',
  image_unavailable: '镜像不可用',
  instance_already_managed: '实例已被纳管',
  instance_busy: '资源正在执行其他操作，请稍后',
  instance_identity_changed: '实例身份已变化，请重新探测',
  instance_not_ready: '实例尚未就绪',
  instance_unavailable: '实例未运行或不可运维',
  internal_error: '服务器内部错误',
  invalid_credentials: '用户名或密码错误',
  invalid_cursor: '分页已失效，已回到第一页',
  invalid_field: '请求字段不正确，请检查填写内容',
  invalid_gpu_uuid: 'GPU 标识不正确，请重新选择',
  invalid_json: '请求格式不正确，请刷新页面',
  invalid_kind: '请求类型不正确，请检查选择',
  invalid_network: '网络配置不正确，请检查填写内容',
  invalid_origin: '请求来源不被允许',
  invalid_policy: '政策配置不正确，请检查填写内容',
  invalid_query: '查询条件不正确，请调整筛选条件',
  invalid_range: '时间范围不正确',
  invalid_request: '请求参数不正确，请检查填写内容',
  invalid_response: '服务返回的数据格式异常，请联系管理员',
  invalid_size: '资源规格不正确，请检查填写内容',
  invalid_state: '当前状态不允许该操作',
  invalid_time: '时间格式不正确',
  validation_failed: '填写内容未通过校验，请检查字段与配置',
  registration_disabled: '当前未开放注册，请联系管理员',
  username_taken: '该用户名已被使用，请更换用户名',
  ip_capacity_unavailable: '没有可分配的 IP 地址',
  metrics_unavailable: '暂无采样数据',
  network_error: '网络连接失败',
  network_unavailable: '网络未就绪',
  network_unverified: '网络绑定尚未验证',
  node_capacity_unavailable: '节点资源不足',
  node_unavailable: '节点不可用（未启用、离线或库存过期），请先探测库存',
  not_found: '资源不存在或无权访问',
  not_ready: '服务暂不可用',
  observation_incomplete: '缺少完整的观测数据，请稍后重试',
  observation_superseded: '观测已被更新的结果取代，请重试',
  owner_inactive: '实例所有者账号已停用',
  plan_changed: '套餐配置已变化，请重新加载套餐并重新报价后确认',
  policy_changed: '价格或政策已变化，请重新确认',
  policy_not_configured: '平台尚未配置所需政策',
  pool_in_use: '地址池仍在使用，不能修改',
  pool_range_in_use:
    '新分配范围不能排除已登记的 IP（包括已绑定、预留及空闲地址）',
  precondition_required: '缺少版本号，请刷新后重试',
  quota_exceeded: '超出资源配额',
  quota_not_configured: '账号尚未配置配额',
  rate_limited: '操作过于频繁',
  resident_capacity_unavailable: '节点常驻资源不足',
  revision_conflict: '数据已被其他操作修改，已为你刷新',
  service_unavailable: '服务暂不可用',
  stale_validation: '网络验证已过期，请重新验证',
  storage_pool_capacity_exceeded:
    '存储池磁盘空间不足，请减小磁盘申请量或联系管理员调整存储',
  task_failed: '任务执行失败，请查看任务详情并修正原因；不会自动重试',
  task_force_stopped: '任务编排已强制停止；已发出的外部操作不保证停止，请按任务提示核对',
  task_stop_not_allowed: '系统执行任务仅管理员可强停',
  task_lease_lost: '任务处理已转交，请刷新',
  task_not_unknown: '只有结果未知的任务可以核对',
  unauthenticated: '登录已过期，请重新登录',
  unsupported_configuration: '当前配置不支持此操作，请先停止实例',
  unsupported_resolution: '不支持的数据粒度',
};

const INITIALIZATION_MESSAGES: Record<string, string> = {
  initialization_cloud_init_degraded: 'cloud-init 已带警告完成',
  initialization_cloud_init_failed: 'cloud-init 初始化失败',
  initialization_password_not_ready: 'root 账号密码或所提供的 SSH 公钥尚未就绪，请联系管理员核对',
  initialization_tool_missing: '初始化所需工具缺失',
  initialization_marker_missing: 'cloud-init 启动完成标记缺失，尚不能确认完成',
  initialization_script_failed: '初始化脚本执行失败',
  initialization_timeout: '等待初始化结果超时，请联系管理员核对；不要重复创建',
  initialization_result_unknown: '初始化结果尚未确认，请联系管理员核对；不会自动重新执行',
  initialization_diagnostic_unavailable: '初始化诊断暂不可用，请联系管理员核对',
};
const INITIALIZATION_REASONS: Record<string, string> = {
  cloud_init_degraded: 'cloud-init 已完成，但报告可恢复警告',
  cloud_init_errors: 'cloud-init 报告初始化错误',
  root_password_not_ready: '未能核验 root 账号密码已正确设置且无需首次登录强制改密，或所提供的 SSH 公钥未正确写入；不表示 SSH 服务是否就绪',
  required_tool_missing: '缺少初始化必需工具',
  ssh_host_key_generation_missing_tool: 'SSH 主机密钥生成失败：缺少 ssh-keygen。请检查镜像的 cloud-init SSH 模块配置；网络与 root 账号凭据配置不要求 SSH 服务',
  completion_marker_missing: '未找到 cloud-init 启动完成标记',
  script_exit_nonzero: '初始化脚本以非零退出码结束',
  command_terminated: '初始化命令被终止，请核对是否存在超时或资源限制等原因',
  template_timeout_invalid: '初始化模板超时配置无效',
  timeout: '等待初始化完成超时',
  result_unknown: '初始化执行结果待核对',
  diagnostic_unavailable: '无法取得可靠的初始化诊断',
  template_hash_mismatch: '初始化模板校验不一致',
  template_status_mismatch: '初始化模板状态不一致',
};
const INITIALIZATION_STAGES: Record<string, string> = {
  'login.cloud-ready': '网络与 root 账号凭据检查',
  'init.cloud-ready': '实例初始化完成检查',
  'init.template': '初始化模板执行',
};
const CLOUD_INIT_STATUSES: Record<string, string> = {
  not_run: '尚未运行', running: '运行中', done: '已完成', degraded: '已降级',
  error: '失败', disabled: '已禁用', unknown: '待核对',
};

// 初始化错误使用独立白名单，不复用可包含任意业务文本的 details/action。
const InitializationDetailsSchema = z.object({
  stage: z.string().optional().catch(undefined),
  reason: z.string().optional().catch(undefined),
  cloud_init_status: z.string().optional().catch(undefined),
  exit_code: z.number().int().optional().catch(undefined),
});

function initializationLabel(labels: Record<string, string>, key?: string): string {
  return key && Object.hasOwn(labels, key) ? labels[key]! : '';
}

const DecimalSchema = z.string().regex(/^-?\d+(?:\.\d+)?$/);
const FieldIssueSchema = z.object({ field: z.string(), message: z.string() });

/** 只解释公开契约中的字段，不将任意 details 对象/上游内容打印给用户。 */
export const ErrorDetailsSchema = z.object({
  action: z.string().optional(),
  available: DecimalSchema.optional(),
  field: z.string().optional(),
  fields: z
    .union([
      z.record(z.union([z.string(), z.array(z.string())])),
      z.array(FieldIssueSchema),
    ])
    .optional(),
  limit: DecimalSchema.optional(),
  pool: z.string().optional(),
  range_start: z.string().optional(),
  range_end: z.string().optional(),
  subnet: z.string().optional(),
  requested: DecimalSchema.optional(),
  resource: z.string().optional(),
  scope: z.string().optional(),
  stage: z.string().optional(),
  total: DecimalSchema.optional(),
  unit: z.enum(['bytes', 'count', 'pt']).optional(),
  used: DecimalSchema.optional(),
});

const SCOPES: Record<string, string> = {
  gpu: 'GPU 资源',
  ip_pool: 'IP 地址池',
  node_capacity: '节点容量',
  resident_capacity: '节点常驻容量',
  storage_pool: '存储池',
  user_quota: '用户配额',
};
const RESOURCES: Record<string, string> = {
  addresses: 'IP 地址数',
  disk_bytes: '磁盘',
  gpus: 'GPU 数',
  instances: '实例数',
  memory_bytes: '内存',
  resident_instances: '常驻实例数',
  vcpus: 'vCPU',
};

export interface ErrorText {
  code: string;
  /** 可操作的补充说明，与主提示分开展示。 */
  details: string[];
  requestId: string;
  stage: string;
  taskCode?: string;
  text: string;
}

const hasChinese = (text: string) => /[\u3400-\u9fff]/.test(text);
const GENERIC_MESSAGE = '操作未能完成，请联系管理员并提供请求 ID';

function publicErrorText(error: PublicError, requestId = ''): ErrorText {
  if (error.code.startsWith('initialization_')) {
    const parsed = InitializationDetailsSchema.safeParse(error.details);
    const d = parsed.success ? parsed.data : undefined;
    const details: string[] = [];
    const reason = initializationLabel(INITIALIZATION_REASONS, d?.reason);
    const status = initializationLabel(CLOUD_INIT_STATUSES, d?.cloud_init_status);
    if (reason) details.push(`原因：${reason}`);
    if (status) details.push(`cloud-init 状态：${status}`);
    if (d?.exit_code !== undefined) details.push(`退出码：${d.exit_code}`);
    details.push('请联系管理员检查镜像依赖与初始化配置，并核对实例实际状态；不会自动重试。');
    return {
      code: error.code, details, requestId,
      stage: initializationLabel(INITIALIZATION_STAGES, d?.stage),
      text: initializationLabel(INITIALIZATION_MESSAGES, error.code) || GENERIC_MESSAGE,
    };
  }
  const message = error.message.trim().replace(
    /(-?\d+(?:\.\d+)?)\s*(pt)\b/gi,
    (_, value: string, unit: string) => `${formatPt(value)} ${unit}`,
  );
  // 后端公开的中文业务说明优先；历史英文 message 使用中文码表兜底。
  const fullText =
    (hasChinese(message) ? message : MESSAGES[error.code]) || GENERIC_MESSAGE;
  const boundary = fullText.search(/[；。\n]/);
  const end =
    boundary > 0 && boundary < 100
      ? boundary + 1
      : Math.min(fullText.length, 100);
  const text = fullText.slice(0, end);
  const details: string[] = [];
  const append = (line: string) => {
    const formatted = line.replace(
      /(-?\d+(?:\.\d+)?)\s*(pt)\b/gi,
      (_, value: string, unit: string) => `${formatPt(value)} ${unit}`,
    );
    if (formatted && !fullText.includes(formatted) && !details.includes(formatted))
      details.push(formatted);
  };
  if (end < fullText.length) details.push(fullText.slice(end).trim());
  const parsed = ErrorDetailsSchema.safeParse(error.details);
  const d = parsed.success ? parsed.data : null;
  if (d) {
    const scope = d.scope ? SCOPES[d.scope] : undefined;
    const resource = d.resource ? RESOURCES[d.resource] : undefined;
    append(
      [scope, resource]
        .filter((part) => part && !fullText.includes(part))
        .join(' · '),
    );
    if (d.pool && !fullText.includes(d.pool)) append(`存储池：${d.pool}`);
    if (d.range_start && d.range_end)
      append(`允许登记范围：${d.range_start} ～ ${d.range_end}`);
    if (d.subnet) append(`所属网段：${d.subnet}`);
    // 逐项去重：message 只写申请量时，仍显示上限、已用和可用量。
    const figureLabels = {
      available: /可用|剩余|空闲/,
      limit: /上限|总量|总容量|总计|总共|配额|总空间/,
      requested: /申请|请求|需要|本次/,
      used: /已用|已使用|已占用/,
      total: /上限|总量|总容量|总计|总共|总空间/,
    };
    const clauses = fullText.split(/[，；。\n]/);
    const figures = (
      [
        ['used', '已用'],
        ['requested', '本次申请'],
        ['limit', '上限'],
        ['total', '总容量'],
        ['available', '可用'],
      ] as const
    ).flatMap(([key, label]) => {
      const value = d[key];
      if (value === undefined) return [];
      if (key === 'total' && value === d.limit) return [];
      const formatted =
        d.unit === 'bytes'
          ? formatBytes(value)
          : d.unit === 'pt'
            ? `${formatPt(value)} pt`
            : value;
      const alreadyShown = clauses.some(
        (clause) =>
          figureLabels[key].test(clause) &&
          ((clause.match(/-?\d+(?:\.\d+)?/g) ?? []).some(
            (token) => token === value,
          ) ||
            (
              clause.match(/≈?-?\d+(?:\.\d+)?\s*(?:GiB|MiB|B|pt)\b/gi) ?? []
            ).some(
              (token) =>
                token.replace(/\s/g, '').toLowerCase() === formatted.replace(/\s/g, '').toLowerCase(),
            )),
      );
      return alreadyShown ? [] : [`${label} ${formatted}`];
    });
    append(figures.join('；'));
    if (d.field && !fullText.includes(d.field)) append(`字段：${d.field}`);
    if (d.fields) {
      const issues = Array.isArray(d.fields)
        ? d.fields
        : Object.entries(d.fields).flatMap(([field, messages]) =>
            (Array.isArray(messages) ? messages : [messages]).map((issue) => ({
              field,
              message: issue,
            })),
          );
      for (const issue of issues) {
        if (hasChinese(issue.message))
          append(`${issue.field}：${issue.message}`);
        else append(`请检查字段：${issue.field}`);
      }
    }
    if (d.action && hasChinese(d.action)) append(d.action);
  }
  return { code: error.code, details, requestId, stage: d?.stage ?? '', text };
}

export function describeError(error: unknown): ErrorText {
  if (error instanceof ApiError) {
    const result = publicErrorText(error, error.requestId);
    if (error.code === 'rate_limited' && error.retryAfter) {
      result.details.push(`请 ${error.retryAfter} 秒后重试`);
    }
    return result;
  }
  const parsed = PublicErrorSchema.safeParse(error);
  if (parsed.success) return publicErrorText(parsed.data);
  // 非协议异常可能包含内部细节，不展示原始 Error.message/String(error)。
  return {
    code: 'client_error',
    details: [],
    requestId: '',
    stage: '',
    text: GENERIC_MESSAGE,
  };
}

export function describeTaskError(
  input: unknown,
  requestId = '',
): ErrorText | null {
  const parsed = TaskErrorSourceSchema.safeParse(input);
  if (!parsed.success) {
    return publicErrorText(
      {
        code: 'invalid_response',
        details: null,
        message: '任务错误数据格式异常，请联系管理员',
      },
      requestId,
    );
  }
  const task = parsed.data;
  if (task.error) {
    return {
      ...describeError(task.error),
      requestId,
      taskCode:
        task.error_code && task.error_code !== task.error.code
          ? task.error_code
          : undefined,
    };
  }
  if (!task.error_code && !['failed', 'unknown'].includes(task.status))
    return null;
  return publicErrorText(
    {
      code:
        task.error_code ||
        (task.status === 'unknown' ? 'external_result_unknown' : 'task_failed'),
      details: null,
      message: '',
    },
    requestId,
  );
}
