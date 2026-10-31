/**
 * 后端接口封装。路径、请求体严格对应 backend/openapi.json 与后端代码。
 * 写操作：调用方传入幂等键（一次用户意图一个）与 If-Match。
 */
import type {
  AuditLog,
  ConsoleKind,
  ConsoleSession,
  CreditAccount,
  CreditEnforcement,
  CustomConfig,
  GPUAllocation,
  GPUOption,
  GPUPolicy,
  GPUPolicyConfig,
  ID,
  Image,
  InitTemplate,
  InitTemplateInput,
  Instance,
  InstanceNetwork,
  IPAddress,
  LatestMetrics,
  LedgerEntry,
  Limits,
  LimitsInput,
  Me,
  MetricsHistory,
  NetworkBinding,
  NetworkMode,
  NetworkOption,
  NetworkPool,
  Node,
  NodeConnectionInput,
  NodeGPU,
  NodeResources,
  Page,
  Plan,
  PlanInput,
  PlatformBindings,
  PolicyKind,
  PolicyVersion,
  PolicyVersions,
  RedemptionCode,
  Resources,
  Task,
  TaskAccepted,
  UnmanagedInstance,
  User,
} from './types';
import type { ApiResult } from './request';

import { api, ApiError } from './request';
import { TaskPageSchema, TaskSchema } from './task-schema';

export type Query = Record<string, boolean | number | string | undefined>;

/** 写操作公共参数：幂等键与 If-Match。 */
export interface Write {
  ifMatch?: null | string;
  key?: string;
}

function clean(q: Query = {}): Query {
  const out: Query = {};
  for (const [k, v] of Object.entries(q)) {
    if (v !== undefined && v !== '' && v !== null) out[k] = v;
  }
  return out;
}

const list = <T>(url: string, q?: Query) =>
  api.get<Page<T>>(url, { params: clean(q) });
const detail = <T>(url: string) => api.get<T>(url);
const post = <T>(url: string, body: unknown, w: Write = {}) =>
  api.post<T>(url, { body, idempotencyKey: w.key, ifMatch: w.ifMatch });
const patch = <T>(url: string, body: unknown, w: Write) =>
  api.patch<T>(url, { body, ifMatch: w.ifMatch });
const put = <T>(url: string, body: unknown, w: Write) =>
  api.put<T>(url, { body, ifMatch: w.ifMatch });
const del = <T>(url: string, body: unknown, w: Write) =>
  api.delete<T>(url, { body, idempotencyKey: w.key, ifMatch: w.ifMatch });

/* ---------------- 认证与本人 ---------------- */

export const authApi = {
  login: (username: string, password: string) =>
    api.post<{ csrf_token: string; user: User }>('/auth/login', {
      body: { password, username },
      silentAuth: true,
    }),
  logout: () => api.post('/auth/logout', { body: {}, silentAuth: true }),
  me: (silent = false) => api.get<Me>('/me', { silentAuth: silent }),
};

/* ---------------- 管理端：用户 ---------------- */

export const usersApi = {
  create: (
    body: {
      email: null | string;
      initial_password: string;
      role: 'admin' | 'user';
      username: string;
    },
    w: Write,
  ) => post<User>('/admin/users', body, w),
  get: (id: ID) => detail<User>(`/admin/users/${id}`),
  limits: (id: ID) => detail<Limits>(`/admin/users/${id}/limits`),
  list: (q?: Query) => list<User>('/admin/users', q),
  putLimits: (id: ID, body: LimitsInput, w: Write) =>
    put<Limits>(`/admin/users/${id}/limits`, body, w),
  resetPassword: (id: ID, body: { new_password: string }, w: Write) =>
    post<null>(`/admin/users/${id}/reset-password`, body, w),
  update: (
    id: ID,
    body: Partial<{
      email: null | string;
      role: 'admin' | 'user';
      status: 'active' | 'disabled';
    }>,
    w: Write,
  ) => patch<User>(`/admin/users/${id}`, body, w),
};

/* ---------------- 管理端：节点与 GPU ---------------- */

export interface NodeCreate {
  allocatable: Node['allocatable'];
  connection: NodeConnectionInput;
  defaults: Node['defaults'];
  name: string;
}

export const nodesApi = {
  create: (body: NodeCreate, w: Write) => post<Node>('/admin/nodes', body, w),
  drain: (id: ID, w: Write) =>
    post<TaskAccepted>(
      `/admin/nodes/${id}/drain`,
      { mode: 'prevent_new_allocations' },
      w,
    ),
  get: (id: ID) => detail<Node>(`/admin/nodes/${id}`),
  gpu: (id: ID, gpuId: ID) =>
    detail<NodeGPU>(`/admin/nodes/${id}/gpus/${gpuId}`),
  gpus: (id: ID, q?: Query) => list<NodeGPU>(`/admin/nodes/${id}/gpus`, q),
  importInstance: (
    id: ID,
    body: {
      approved_config: CustomConfig;
      incus_uuid: string;
      owner_user_id: ID;
    },
    w: Write,
  ) => post<TaskAccepted>(`/admin/nodes/${id}/instance-imports`, body, w),
  list: (q?: Query) => list<Node>('/admin/nodes', q),
  probe: (id: ID, w: Write) =>
    post<TaskAccepted>(`/admin/nodes/${id}/probe`, {}, w),
  replaceConnection: (
    id: ID,
    body: { connection: NodeConnectionInput },
    w: Write,
  ) => put<Node>(`/admin/nodes/${id}/connection`, body, w),
  resources: (id: ID) => detail<NodeResources>(`/admin/nodes/${id}/resources`),
  unmanaged: (id: ID, q?: Query) =>
    list<UnmanagedInstance>(`/admin/nodes/${id}/unmanaged-instances`, q),
  update: (
    id: ID,
    body: Partial<{
      administrative_state: Node['administrative_state'];
      allocatable: Node['allocatable'];
      name: string;
    }>,
    w: Write,
  ) => patch<Node>(`/admin/nodes/${id}`, body, w),
  updateGPU: (
    id: ID,
    gpuId: ID,
    body: Partial<{
      gpu_price_version_id: ID | null;
      max_allocations: number;
      status: NodeGPU['status'];
    }>,
    w: Write,
  ) => patch<NodeGPU>(`/admin/nodes/${id}/gpus/${gpuId}`, body, w),
};

/* ---------------- 管理端：网络 ---------------- */

export const networkApi = {
  addAddresses: (poolId: ID, addresses: string[], w: Write) =>
    post<{ registered: string }>(
      `/admin/network-pools/${poolId}/addresses`,
      { addresses },
      w,
    ),
  addresses: (poolId: ID, q?: Query) =>
    list<IPAddress>(`/admin/network-pools/${poolId}/addresses`, q),
  binding: (id: ID) =>
    detail<NetworkBinding>(`/admin/node-network-bindings/${id}`),
  bindings: (q?: Query) =>
    list<NetworkBinding>('/admin/node-network-bindings', q),
  createBinding: (
    body: {
      incus_network_name?: string;
      mode: NetworkMode;
      node_id: ID;
      parent_interface?: string;
      pool_id: ID;
      project_name: string;
    },
    w: Write,
  ) => post<NetworkBinding>('/admin/node-network-bindings', body, w),
  createPool: (
    body: {
      cidr: string;
      dns_servers: string[];
      gateway: string;
      name: string;
      range_end: string;
      range_start: string;
    },
    w: Write,
  ) => post<NetworkPool>('/admin/network-pools', body, w),
  pool: (id: ID) => detail<NetworkPool>(`/admin/network-pools/${id}`),
  pools: (q?: Query) => list<NetworkPool>('/admin/network-pools', q),
  updateBinding: (id: ID, status: 'active' | 'disabled', w: Write) =>
    patch<NetworkBinding>(`/admin/node-network-bindings/${id}`, { status }, w),
  updatePool: (
    id: ID,
    body: Partial<{
      dns_servers: string[];
      gateway: string;
      name: string;
      range_start: string;
      range_end: string;
      status: 'active' | 'disabled';
    }>,
    w: Write,
  ) => patch<NetworkPool>(`/admin/network-pools/${id}`, body, w),
  validateBinding: (id: ID, w: Write) =>
    post<TaskAccepted>(`/admin/node-network-bindings/${id}/validate`, {}, w),
};

/* ---------------- 管理端：实例 ---------------- */

export type PowerMode = 'force' | 'graceful';

export interface AttachBody {
  access_mode: 'exclusive' | 'shared';
  allow_live: boolean;
  count: number;
  fixed_uuids?: string[];
  max_hourly_pt: string;
  model: null | string;
}

const ai = (id: ID, path = '') => `/admin/instances/${id}${path}`;

export const instancesApi = {
  attachGPU: (id: ID, body: AttachBody, w: Write) =>
    post<TaskAccepted>(ai(id, '/gpu-attachments'), body, w),
  createCustom: (
    body: { approved_config: CustomConfig; name: string; owner_user_id: ID },
    w: Write,
  ) => post<TaskAccepted>('/admin/custom-instances', body, w),
  delete: (
    id: ID,
    body: { confirm_name: string; delete_data: true },
    w: Write,
  ) => del<TaskAccepted>(ai(id), body, w),
  detachGPU: (
    id: ID,
    body: { allocation_ids: ID[]; allow_live: boolean },
    w: Write,
  ) => post<TaskAccepted>(ai(id, '/gpu-detachments'), body, w),
  get: (id: ID) => detail<Instance>(ai(id)),
  gpuAllocations: (id: ID) =>
    detail<GPUAllocation[]>(ai(id, '/gpu-allocations')),
  gpuPolicy: (id: ID) => detail<GPUPolicy>(ai(id, '/gpu-policy')),
  history: (id: ID, q: Query) =>
    api.get<MetricsHistory>(ai(id, '/metrics/history'), { params: clean(q) }),
  latest: (id: ID) => detail<LatestMetrics>(ai(id, '/metrics/latest')),
  list: (q?: Query) => list<Instance>('/admin/instances', q),
  network: (id: ID) => detail<InstanceNetwork[]>(ai(id, '/network')),
  power: (
    id: ID,
    action: 'restart' | 'start' | 'stop',
    body: { confirm_data_loss_risk?: boolean; mode?: PowerMode },
    w: Write,
  ) => post<TaskAccepted>(ai(id, `/${action}`), body, w),
  putGPUPolicy: (
    id: ID,
    body: GPUPolicyConfig & {
      allowed_user_actions?: string[];
      gpu_user_permissions?: Record<string, boolean>;
    },
    w: Write,
  ) => put<GPUPolicy>(ai(id, '/gpu-policy'), body, w),
  rebindGPU: (
    id: ID,
    body: {
      allocation_ids: ID[];
      allow_live: boolean;
      fixed_uuids?: string[];
      max_hourly_pt: string;
    },
    w: Write,
  ) => post<TaskAccepted>(ai(id, '/gpu-rebindings'), body, w),
  rename: (id: ID, name: string, w: Write) =>
    patch<Instance>(ai(id), { name }, w),
  resize: (
    id: ID,
    body: { policy_versions?: PolicyVersions; resources: Resources },
    w: Write,
  ) => post<TaskAccepted>(ai(id, '/resize'), body, w),
};

/* ---------------- 管理端：控制台 ---------------- */

export const consoleApi = {
  close: (sessionId: string, w: Write) =>
    del<null>(`/admin/console-sessions/${sessionId}`, {}, w),
  create: (
    instanceId: ID,
    body: { cols?: number; kind: ConsoleKind; rows?: number },
    w: Write,
  ) => post<ConsoleSession>(ai(instanceId, '/console-sessions'), body, w),
  get: (sessionId: string) =>
    detail<ConsoleSession>(`/admin/console-sessions/${sessionId}`),
};

/* ---------------- 管理端：产品目录 ---------------- */

export const catalogApi = {
  createImage: (
    body: {
      architecture: string;
      code: string;
      fingerprint: string;
      instance_type: Image['instance_type'];
      properties: { gpu_ready: boolean; os_name: string; os_release: string };
      version: string;
    },
    w: Write,
  ) => post<Image>('/admin/images', body, w),
  createPlan: (body: PlanInput, w: Write) =>
    post<Plan>('/admin/plans', body, w),
  createTemplate: (body: InitTemplateInput, w: Write) =>
    post<InitTemplate>('/admin/init-templates', body, w),
  gpuOptions: (q?: Query) => list<GPUOption>('/catalog/gpu-options', q),
  image: (id: ID) => detail<Image>(`/admin/images/${id}`),
  images: (q?: Query) => list<Image>('/admin/images', q),
  networkOptions: (q: Query) =>
    list<NetworkOption>('/catalog/network-options', q),
  plan: (id: ID) => detail<Plan>(`/admin/plans/${id}`),
  plans: (q?: Query) => list<Plan>('/admin/plans', q),
  publishPlan: (id: ID, w: Write) =>
    post<Plan>(`/admin/plans/${id}/publish`, {}, w),
  publishTemplate: (id: ID, w: Write) =>
    post<InitTemplate>(`/admin/init-templates/${id}/publish`, {}, w),
  retirePlan: (id: ID, w: Write) =>
    post<Plan>(`/admin/plans/${id}/retire`, {}, w),
  retireTemplate: (id: ID, w: Write) =>
    post<InitTemplate>(`/admin/init-templates/${id}/retire`, {}, w),
  template: (id: ID) => detail<InitTemplate>(`/admin/init-templates/${id}`),
  templates: (q?: Query) => list<InitTemplate>('/admin/init-templates', q),
  updateImage: (
    id: ID,
    body: Partial<{ properties: Image['properties']; status: Image['status'] }>,
    w: Write,
  ) => patch<Image>(`/admin/images/${id}`, body, w),
  /** 后端只接受草稿套餐的这些字段；code/version/status 不可改。 */
  updatePlan: (
    id: ID,
    body: Partial<Omit<PlanInput, 'code' | 'version'>>,
    w: Write,
  ) => patch<Plan>(`/admin/plans/${id}`, body, w),
  /** 后端只接受草稿模板的 name/script/parameter_schema/timeout_seconds/phase。 */
  updateTemplate: (
    id: ID,
    body: Partial<Omit<InitTemplateInput, 'code' | 'version'>>,
    w: Write,
  ) => patch<InitTemplate>(`/admin/init-templates/${id}`, body, w),
};

/* ---------------- 管理端：政策 ---------------- */

const pv = (kind: PolicyKind, path = '') =>
  `/admin/policies/${kind}/versions${path}`;

export const policiesApi = {
  bindings: () => detail<PlatformBindings>('/admin/platform-policy-bindings'),
  create: (
    kind: PolicyKind,
    body: { code: string; config: Record<string, unknown>; version: string },
    w: Write,
  ) => post<PolicyVersion>(pv(kind), body, w),
  get: (kind: PolicyKind, id: ID) => detail<PolicyVersion>(pv(kind, `/${id}`)),
  list: (kind: PolicyKind, q?: Query) => list<PolicyVersion>(pv(kind), q),
  publish: (kind: PolicyKind, id: ID, effective_at: string, w: Write) =>
    post<PolicyVersion>(pv(kind, `/${id}/publish`), { effective_at }, w),
  putBindings: (body: Record<PolicyKind, ID>, w: Write) =>
    put<PlatformBindings>('/admin/platform-policy-bindings', body, w),
  retire: (kind: PolicyKind, id: ID, w: Write) =>
    post<PolicyVersion>(pv(kind, `/${id}/retire`), {}, w),
  update: (
    kind: PolicyKind,
    id: ID,
    config: Record<string, unknown>,
    w: Write,
  ) => patch<PolicyVersion>(pv(kind, `/${id}`), { config }, w),
};

/* ---------------- 管理端：财务 ---------------- */

export const financeApi = {
  account: (id: ID) => detail<CreditAccount>(`/admin/credit-accounts/${id}`),
  accounts: (q?: Query) => list<CreditAccount>('/admin/credit-accounts', q),
  adjust: (
    id: ID,
    body: { amount_pt: string; expires_at: null | string },
    w: Write,
  ) =>
    post<Record<string, unknown>>(
      `/admin/credit-accounts/${id}/adjustments`,
      body,
      w,
    ),
  codes: (q?: Query) => list<RedemptionCode>('/admin/redemption-codes', q),
  createCodes: (
    body: {
      amount_pt: string;
      count: number;
      expires_at: string;
      max_redemptions: number;
    },
    w: Write,
  ) =>
    post<{
      codes_revealable?: boolean;
      items?: RedemptionCode[];
      record_ids?: ID[];
    }>('/admin/redemption-codes', body, w),
  disableCode: (id: ID, w: Write) =>
    post<RedemptionCode>(`/admin/redemption-codes/${id}/disable`, {}, w),
  enforcement: (id: ID) =>
    detail<CreditEnforcement>(`/admin/credit-enforcements/${id}`),
  enforcements: (q?: Query) =>
    list<CreditEnforcement>('/admin/credit-enforcements', q),
  extend: (id: ID, body: { shutdown_deadline: string }, w: Write) =>
    post<CreditEnforcement>(`/admin/credit-enforcements/${id}/extend`, body, w),
  ledger: (q?: Query) => list<LedgerEntry>('/admin/credit-ledger', q),
  reconcileEnforcement: (id: ID, w: Write) =>
    post<TaskAccepted>(`/admin/credit-enforcements/${id}/reconcile`, {}, w),
  switchPolicy: (id: ID, body: { policy_version_id: ID }, w: Write) =>
    patch<CreditAccount>(`/admin/credit-accounts/${id}`, body, w),
};

/* ---------------- 管理端：任务与审计 ---------------- */

export const opsApi = {
  audit: (q?: Query) => list<AuditLog>('/admin/audit-logs', q),
  reconcile: (id: ID, w: Write) =>
    post<TaskAccepted>(`/admin/tasks/${id}/reconcile`, {}, w),
  task: async (id: ID): Promise<ApiResult<Task>> => {
    const result = await detail<unknown>(`/admin/tasks/${id}`);
    const parsed = TaskSchema.safeParse(result.data);
    if (!parsed.success) {
      throw new ApiError(result.status, { code: 'invalid_response', details: null, message: '任务数据格式异常，请联系管理员' }, result.requestId, null);
    }
    return { ...result, data: parsed.data };
  },
  tasks: async (q?: Query): Promise<ApiResult<Page<Task>>> => {
    const result = await api.get<unknown>('/admin/tasks', { params: clean(q) });
    const parsed = TaskPageSchema.safeParse(result.data);
    if (!parsed.success) {
      throw new ApiError(result.status, { code: 'invalid_response', details: null, message: '任务列表数据格式异常，请联系管理员' }, result.requestId, null);
    }
    return { ...result, data: parsed.data };
  },
};

/** 取完游标分页的全部项（用于下拉选项等小集合，最多 pages 页）。 */
export async function listAll<T>(
  fetcher: (q: Query) => Promise<ApiResult<Page<T>>>,
  q: Query = {},
  pages = 10,
): Promise<T[]> {
  const out: T[] = [];
  let cursor: string | undefined;
  for (let i = 0; i < pages; i++) {
    const r = await fetcher({ ...q, cursor, limit: 100 });
    out.push(...r.data.items);
    if (!r.data.next_cursor) break;
    cursor = r.data.next_cursor;
  }
  return out;
}
