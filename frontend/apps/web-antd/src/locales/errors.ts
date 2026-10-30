/** 后端错误码中文提示，见 frontend/前端设计.md 第 12 节。 */
import { ApiError } from '#/api/request';

const MESSAGES: Record<string, string> = {
  action_not_allowed: '管理员未授权此实例执行该操作',
  agent_invalid_response: '节点返回异常',
  capacity_in_use: '资源仍在使用，不能修改',
  configuration_conflict: '实际配置与记录不一致',
  console_expired: '控制台会话已过期',
  console_limit: '同时打开的控制台已达上限',
  console_not_ready: '控制台会话不可用，请重新连接',
  console_process_mismatch: '控制台会话不可用，请重新连接',
  console_revoked: '控制台权限已失效',
  console_unsupported: '该实例不支持此控制台类型',
  console_used: '控制台会话已被使用，请重新连接',
  credit_exhausted: '可用额度不足',
  csrf_failed: '页面已过期，请刷新后重试',
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
  invalid_origin: '请求来源不被允许',
  invalid_range: '时间范围不正确',
  invalid_state: '当前状态不允许该操作',
  invalid_time: '时间格式不正确',
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
  policy_changed: '价格或政策已变化，请重新确认',
  policy_not_configured: '平台尚未配置所需政策',
  pool_in_use: '地址池仍在使用，不能修改',
  precondition_required: '缺少版本号，请刷新后重试',
  quota_exceeded: '超出资源配额',
  quota_not_configured: '账号尚未配置配额',
  rate_limited: '操作过于频繁',
  request_not_pending: '申请已处理，不能再修改',
  resident_capacity_unavailable: '节点常驻资源不足',
  revision_conflict: '数据已被其他操作修改，已为你刷新',
  service_unavailable: '服务暂不可用',
  stale_validation: '网络验证已过期，请重新验证',
  task_lease_lost: '任务处理已转交，请刷新',
  task_not_unknown: '只有结果未知的任务可以核对',
  unauthenticated: '登录已过期，请重新登录',
  unsupported_configuration: '当前配置不支持此操作，请先停止实例',
  unsupported_resolution: '不支持的数据粒度',
  zone_unavailable: '可用区不可用',
};

/** 这些错误码的后端 message 比通用提示更具体，直接展示原文。 */
const SHOW_BACKEND_MESSAGE: Record<string, true> = {
  conflict: true,
  invalid_field: true,
  invalid_gpu_uuid: true,
  invalid_kind: true,
  invalid_network: true,
  invalid_policy: true,
  invalid_query: true,
  invalid_request: true,
  invalid_size: true,
  reason_required: true,
  validation_failed: true,
};

export interface ErrorText {
  code: string;
  requestId: string;
  text: string;
}

export function describeError(error: unknown): ErrorText {
  if (error instanceof ApiError) {
    let text = SHOW_BACKEND_MESSAGE[error.code]
      ? error.message
      : (MESSAGES[error.code] ?? error.message);
    if (error.code === 'rate_limited' && error.retryAfter) {
      text = `${text}，请 ${error.retryAfter} 秒后重试`;
    }
    return { code: error.code, requestId: error.requestId, text };
  }
  return {
    code: 'client_error',
    requestId: '',
    text: error instanceof Error ? error.message : String(error),
  };
}
