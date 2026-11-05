/** 枚举中文与颜色，见 frontend/前端设计.md 附录 A。颜色为 antd Tag color。 */
import type { Instance } from '#/api/types';

export type Tone = 'default' | 'error' | 'processing' | 'success' | 'warning';

export interface EnumLabel {
  color: Tone;
  text: string;
}

type Dict = Record<string, EnumLabel>;

const L = (text: string, color: Tone = 'default'): EnumLabel => ({ color, text });

export const ENUMS: Record<string, Dict> = {
  accountStatus: { active: L('正常', 'success'), closed: L('关闭'), frozen: L('冻结', 'warning') },
  addressStatus: {
    available: L('可用'),
    bound: L('已绑定', 'success'),
    conflict: L('冲突', 'error'),
    quarantined: L('隔离', 'warning'),
    reserved: L('已预留', 'processing'),
  },
  adminState: {
    active: L('启用', 'success'),
    disabled: L('禁用'),
    draining: L('排空中', 'warning'),
    maintenance: L('维护', 'warning'),
  },
  allocation: {
    active: L('使用中', 'success'),
    attaching: L('挂载中', 'processing'),
    cancelled: L('已取消'),
    failed: L('失败', 'error'),
    released: L('已释放'),
    releasing: L('释放中', 'processing'),
    reserved: L('已预留', 'processing'),
    unknown: L('未知', 'warning'),
  },
  billingMode: { admin_exempt: L('免额度', 'processing'), credit: L('额度计费') },
  catalogStatus: {
    active: L('已发布', 'success'),
    disabled: L('停用'),
    draft: L('草稿', 'warning'),
    retired: L('已退役'),
  },
  planStatus: { active: L('启用', 'success'), disabled: L('停用') },
  codeStatus: { active: L('有效', 'success'), disabled: L('已禁用') },
  consoleStatus: {
    active: L('已连接', 'success'),
    closed: L('已关闭'),
    expired: L('已过期'),
    failed: L('失败', 'error'),
    preparing: L('准备中', 'processing'),
    ready: L('就绪', 'processing'),
  },
  enforcement: {
    cancelled: L('已取消'),
    completed: L('已完成'),
    grace: L('宽限中', 'error'),
    stopping: L('停止中', 'error'),
    unknown: L('待核对', 'warning'),
  },
  entryType: {
    adjust: L('调整'),
    expire: L('过期'),
    grant: L('发放', 'success'),
    refund: L('退回'),
    release: L('释放预留'),
    reserve: L('预留', 'processing'),
    settle: L('结算'),
  },
  gpuAttachment: {
    attached: L('已挂载', 'success'),
    attaching: L('挂载中', 'processing'),
    cancelled: L('预留已取消'),
    configuration_missing: L('挂载配置待核对', 'warning'),
    configuration_unknown: L('挂载配置未知', 'warning'),
    failed: L('分配失败', 'error'),
    released: L('已释放'),
    releasing: L('释放中，待确认', 'processing'),
    reserved: L('已预留，待挂载', 'processing'),
    unknown: L('分配结果待核对', 'warning'),
  },
  gpuHealth: {
    error: L('错误', 'error'),
    healthy: L('健康', 'success'),
    missing: L('未找到', 'error'),
    unknown: L('未知', 'warning'),
  },
  gpuStatus: {
    available: L('可用', 'success'),
    disabled: L('禁用'),
    error: L('错误', 'error'),
    maintenance: L('维护', 'warning'),
    missing: L('未找到', 'error'),
  },
  health: { offline: L('离线', 'error'), online: L('在线', 'success'), unknown: L('未知', 'warning') },
  instanceType: { container: L('容器'), 'virtual-machine': L('虚拟机') },
  networkMode: { bridge: L('网桥'), macvlan: L('macvlan 直连'), managed_bridge: L('托管网桥') },
  observed: {
    deleted: L('已删除'),
    error: L('异常', 'error'),
    frozen: L('已冻结', 'warning'),
    pending: L('处理中', 'processing'),
    running: L('运行中', 'success'),
    stopped: L('已停止'),
    unknown: L('未知', 'warning'),
  },
  poolStatus: { active: L('启用', 'success'), disabled: L('停用'), exhausted: L('已用尽', 'warning') },
  productType: { custom: L('自定义', 'processing'), elastic: L('标准') },
  quality: { cached: L('正常', 'success'), stale: L('已过期', 'warning'), unavailable: L('无数据') },
  role: { admin: L('管理员', 'processing'), user: L('成员') },
  simpleStatus: { active: L('启用', 'success'), disabled: L('停用') },
  task: {
    cancelled: L('已取消'),
    failed: L('失败', 'error'),
    queued: L('排队中', 'processing'),
    running: L('执行中', 'processing'),
    succeeded: L('成功', 'success'),
    unknown: L('结果待核对', 'warning'),
    waiting: L('等待中', 'processing'),
  },
  userStatus: { active: L('正常', 'success'), deleted: L('已删除'), disabled: L('已停用', 'warning') },
  validation: { failed: L('验证失败', 'error'), pending: L('待验证', 'warning'), verified: L('已验证', 'success') },
};

export function enumLabel(kind: string, value: null | string | undefined): EnumLabel {
  if (value === null || value === undefined) return L('—');
  return ENUMS[kind]?.[value] ?? L(value);
}

export const TASK_TYPES: Record<string, string> = {
  credit_enforcement_reconcile: '额度事件核对',
  'image.prepare': '准备节点镜像',
  'instance.create': '创建实例',
  'instance.delete': '删除实例',
  'instance.gpu': 'GPU 操作',
  'instance.import': '纳管实例',
  'instance.reconcile': '实例核对',
  'instance.resize': '调整规格',
  'instance.restart': '重启实例',
  'instance.start': '启动实例',
  'instance.stop': '停止实例',
  'monitoring.console.prepare': '准备控制台',
  'network.validate': '网络验证',
  'node.drain': '节点排空',
  'node.probe': '节点探测',
};

export const TASK_NEXT_ACTIONS: Record<string, string> = {
  administrator_reconcile: '请管理员核对实际状态，不会重放原操作',
  correct_and_resubmit: '修正失败原因后可重新提交创建，不会自动重试',
  inspect_failure: '请先检查失败原因与资源实际状态，再决定后续操作',
  none: '无需操作',
  wait_cleanup: '等待平台自动清理，不会重新创建实例',
};

export const AUDIT_EVENTS: Record<string, string> = {
  'api.mutation': '写操作',
  'auth.bootstrap': '初始化管理员',
  'auth.login': '登录',
  'auth.logout': '退出',
  'auth.reset-password': '重置密码',
  'console.connect': '连接控制台',
  'console.create': '创建控制台',
  'console.disconnect': '断开控制台',
  'credit.adjustment': '额度调整',
  'credit.enforcement.extend': '延长停机期限',
  'credit.policy_changed': '切换额度政策',
  'instance.gpu_policy.updated': '修改 GPU 策略',
};

export const QUALITY_REASONS: Record<string, string> = {
  agent_restarted: '节点 Agent 重启',
  gpu_metric_unavailable: '部分 GPU 指标不可用',
  gpu_utilization_no_samples: '暂无进程级SM活动采样',
  gpu_utilization_unknown: '实例 GPU SM 利用率未知',
  instance_attribution_unknown: '实例 GPU 占用归因尚未完整确认',
  instance_not_running: '实例已停止，无运行态进程归因',
  gpu_references_unresolved: 'GPU 占用归属未完全确认',
  history_evicted: '节点历史已被覆盖',
  late_sample: '采样延迟',
  no_retained_observation: '该时段没有保留的数据',
  platform_idle_disabled: '平台未启用空闲回收',
};

export const POLICY_KIND_NAMES: Record<string, string> = {
  credit: '每日额度',
  'gpu-idle': 'GPU 空闲',
  'gpu-pricing': 'GPU 定价',
  'gpu-scheduling': 'GPU 调度',
  metrics: '指标采集',
  pricing: '资源定价',
};

export const CUSTOM_ACTION_NAMES: Record<string, string> = {
  console: '终端',
  delete: '删除',
  gpu_attach: '挂载 GPU',
  gpu_detach: '移除 GPU',
  gpu_policy: 'GPU 策略',
  gpu_rebind: '换卡',
  resize: '调整规格',
  restart: '重启',
  start: '启动',
  stop: '停止',
};

export const GPU_PERMISSION_NAMES: Record<string, string> = {
  allow_live: '允许运行中换卡',
  idle: '空闲回收开关',
  idle_action: '空闲动作',
  idle_timeout: '空闲时长',
  max_hourly_pt: '均衡单价上限',
  rebalance: '自动均衡开关',
};

/** 已确认未派发且释放预留的创建失败优先；其他实例仍以 stale 为先。 */
export function instanceState(i: Pick<Instance, 'observed_state' | 'provisioning_state' | 'resource_reservations_released' | 'state_stale'>): EnumLabel {
  if (i.provisioning_state === 'failed' && i.resource_reservations_released === true) return L('创建失败', 'error');
  if (i.state_stale) return L('状态未知', 'warning');
  switch (i.provisioning_state) {
    case 'creating':
    case 'pending': {
      return L('创建中', 'processing');
    }
    case 'deleting': {
      return L('删除中', 'processing');
    }
    case 'failed': {
      return L('创建失败', 'error');
    }
    case 'initializing': {
      return L('初始化中', 'processing');
    }
    case 'unknown': {
      return L('交付结果未知', 'warning');
    }
    default: {
      return enumLabel('observed', i.observed_state);
    }
  }
}
