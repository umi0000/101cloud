/**
 * 后端 /api/v1 响应类型。字段名、枚举照抄后端 DTO（backend/internal/**），
 * 2026-10-30 在真实服务器上逐个接口核对。ID、字节数、pt 金额是十进制字符串。
 */
import type { PublicError } from './request';

export type ID = string;
/** 十进制字符串，单位 bytes。 */
export type Bytes = string;
/** 十进制字符串，最多 6 位小数。 */
export type Pt = string;
/** RFC3339，+08:00。 */
export type Time = string;

export interface Page<T> {
  items: T[];
  next_cursor: null | string;
}

export type BillingPeriod = 'today' | 'week' | 'month' | 'year';
export interface BillingBreakdown {
  cpu_pt: Pt;
  memory_pt: Pt;
  disk_pt: Pt;
  gpu_pt: Pt;
}
export interface BillingSummary {
  from: Time;
  to: Time;
  consumed_pt: Pt;
  recorded_pt: Pt;
  estimated_pt: Pt;
  breakdown: BillingBreakdown;
}
export interface BillingOverview {
  as_of: Time;
  timezone: string;
  periods: Record<BillingPeriod, BillingSummary>;
  current: { hourly_pt: null | Pt; status: 'ready' | 'unavailable'; reason: null | string };
  forecast: { status: 'estimated' | 'not_consuming' | 'exhausted' | 'unavailable'; seconds: null | string; exhausts_at: null | Time; reason: null | string; basis: 'current_wallet' };
  balance: { available_pt: Pt; reserved_pt: Pt; outstanding_debt_pt: Pt };
  partial: boolean;
  reasons: string[];
}
export interface BillingDistribution extends BillingSummary {
  as_of: Time;
  timezone: string;
  period: BillingPeriod;
  instances: {
    instance_id: ID;
    name: string;
    deleted: boolean;
    instance_state: string;
    consumed_pt: Pt;
    recorded_pt: Pt;
    estimated_pt: Pt;
  }[];
  partial: boolean;
  reasons: string[];
}
export interface BillingTrend {
  as_of: Time;
  timezone: string;
  period: BillingPeriod;
  from: Time;
  to: Time;
  instance_id: ID | null;
  granularity: 'hour' | 'day' | 'month';
  items: { from: Time; to: Time; consumed_pt: null | Pt; recorded_pt: null | Pt; estimated_pt: null | Pt; partial: boolean }[];
  partial: boolean;
  reasons: string[];
}
export interface BillingInstanceOption {
  instance_id: ID;
  name: string;
  deleted: boolean;
}
export interface BillingInstanceOptions {
  as_of: Time;
  page: number;
  page_size: number;
  total: number;
  items: BillingInstanceOption[];
}
export interface BillingInstance {
  instance_id: ID;
  name: string;
  deleted: boolean;
  instance_state: string;
  consumed_pt: Pt;
  recorded_pt: Pt;
  estimated_pt: Pt;
  breakdown: BillingBreakdown;
  current_hourly_pt: null | Pt;
  rate_status: 'ready' | 'unavailable';
}
export interface BillingInstances {
  as_of: Time;
  timezone: string;
  period: BillingPeriod;
  from: Time;
  to: Time;
  page: number;
  page_size: number;
  total: number;
  items: BillingInstance[];
  partial: boolean;
  reasons: string[];
}

export interface TaskAccepted {
  instance_id: ID | null;
  status: TaskStatus;
  task_id: ID;
}

export type Role = 'admin' | 'user';
export type UserStatus = 'active' | 'deleted' | 'disabled';

export interface User {
  created_at: Time;
  email: null | string;
  id: ID;
  revision: string;
  role: Role;
  status: UserStatus;
  username: string;
}

export interface Limits {
  id: ID;
  max_disk_bytes: Bytes;
  max_gpus: number;
  max_instances: number;
  max_memory_bytes: Bytes;
  max_resident_instances: number;
  max_vcpus: number;
  revision: string;
  user_id: ID;
}

export interface LimitsInput {
  max_disk_bytes: Bytes;
  max_gpus: number;
  max_instances: number;
  max_memory_bytes: Bytes;
  max_resident_instances: number;
  max_vcpus: number;
}

export interface RegistrationSettingsInput {
  credit_policy_version_id: ID | null;
  enabled: boolean;
  limits: LimitsInput;
}

export interface RegistrationSettings extends RegistrationSettingsInput {
  id: ID;
  revision: string;
}

export interface Me {
  effective_permissions: string[];
  limits: Limits;
  user: User;
}

export interface Session {
  created_at: Time;
  expires_at: Time;
  id: ID;
  is_current: boolean;
  revision: string;
}

/* ---------- catalog ---------- */

export type InstanceType = 'container' | 'virtual-machine';
export type AccessMode = 'exclusive' | 'shared';
export type CatalogStatus = 'active' | 'disabled' | 'draft' | 'retired';
export type PlanStatus = 'active' | 'disabled';

export interface ImageProperties {
  [key: string]: unknown;
  gpu_ready: boolean;
  os_name: string;
  os_release: string;
}

export type ImageSourceID = 'images' | 'ubuntu';
export interface ImageSource {
  available?: boolean;
  unsupported_reason?: string;
  id: ImageSourceID;
  name: string;
  protocol: 'simplestreams';
  url: string;
}

export interface RemoteImage {
  aliases: string[];
  architecture: string;
  cloud_init: boolean;
  description: string;
  fingerprint: string;
  instance_type: InstanceType;
  os_name: string;
  os_release: string;
  size_bytes: Bytes;
  variant: string;
}

export interface ImageNodeInput {
  architecture: string;
  cloud_init: boolean;
  fingerprint: string;
  gpu_ready: boolean;
  node_id: ID;
  project_name: string;
  properties?: Record<string, unknown>;
  source_id?: ImageSourceID;
  variant?: string;
}

export interface ImageNode extends Omit<ImageNodeInput, 'source_id'> {
  error: null | { code: string; message: string };
  id: ID;
  last_verified_at: null | Time;
  revision: string;
  source_id: ImageSourceID | null;
  status: 'downloading' | 'failed' | 'pending' | 'ready' | 'unknown';
  task_id: ID | null;
}

export interface ImageInput {
  code?: string;
  instance_type: InstanceType;
  nodes: ImageNodeInput[];
  properties: ImageProperties;
  source_type: 'local' | 'remote';
  version?: string;
}

export interface ImageNodesAccepted {
  image_id: ID;
  tasks: { node_id: ID; project_name: string; task_id: ID }[];
}

export interface Image {
  architecture: string;
  code: string;
  created_at: Time;
  fingerprint: string;
  gpu_ready: boolean;
  id: ID;
  instance_type: InstanceType;
  name: string;
  nodes: ImageNode[];
  cloud_init: boolean;
  source_type: 'local' | 'remote';
  os_name: string;
  os_release: string;
  properties: ImageProperties;
  revision: string;
  status: 'active' | 'disabled' | 'retired';
  updated_at: Time;
  version: string;
}

export interface Resources {
  disk_bytes: Bytes;
  memory_bytes: Bytes;
  vcpus: number;
}

export interface ResizeLimits {
  max_resources: Resources;
}

export interface GPURequest {
  access_mode: AccessMode;
  count: number;
  max_hourly_pt: Pt;
  model: null | string;
}

export interface OverrideBound {
  max: string;
  min: string;
  step: string;
}

export type OverrideKey = 'disk_bytes' | 'gpu_count' | 'memory_bytes' | 'vcpus';
export type AllowedOverrides = Partial<Record<OverrideKey, OverrideBound>>;

export type PolicyKind =
  | 'credit'
  | 'gpu-idle'
  | 'gpu-pricing'
  | 'gpu-scheduling'
  | 'metrics'
  | 'pricing';

export const POLICY_KINDS: PolicyKind[] = [
  'pricing',
  'gpu-pricing',
  'credit',
  'gpu-scheduling',
  'gpu-idle',
  'metrics',
];

export type PolicyVersions = Partial<Record<PolicyKind, ID>>;

export interface Plan {
  allowed_overrides: AllowedOverrides;
  allowed_user_actions: CustomAction[];
  code: string;
  created_at: Time;
  gpu: GPURequest;
  gpu_policy: GPUPolicyConfig;
  gpu_user_permissions: Record<string, boolean>;
  id: ID;
  image_ids: ID[];
  init_template_version_id: ID | null;
  instance_type: InstanceType;
  name: string;
  network_option_ids: ID[];
  node_ids: ID[];
  policy_versions: PolicyVersions;
  pricing_version_id: ID | null;
  resources: Resources;
  revision: string;
  status: PlanStatus;
  updated_at: Time;
  version: string;
}

/** 仅用于最终创建请求；不得放入报价、普通配置快照或持久化状态。 */
export type CreateLogin =
  | { root_password: string; ssh_public_keys?: never }
  | { root_password?: never; ssh_public_keys: string[] };

/** 管理员直接下发：登录凭据只在顶层写入，不属于 approved_config。 */
export interface CreateCustomInstance {
  approved_config: CustomConfig;
  login?: CreateLogin;
  name: string;
  owner_user_id: ID;
  policy_versions: PolicyVersions;
}

/** 普通用户弹性实例：节点由网络选项确定，报价及创建绑定套餐 revision。 */
export interface CreateElasticSpec {
  gpu: GPURequest;
  image_id: ID;
  init_parameters: Record<string, unknown>;
  login?: CreateLogin;
  name: string;
  network_option_id: ID;
  plan_id: ID;
  plan_revision: string;
  resources: Resources;
}

/** 报价仅依赖资源配置，不提交名称或登录凭据。 */
export type ElasticQuoteSpec = Omit<CreateElasticSpec, 'login' | 'name'>;

export interface ElasticQuote {
  admissible: boolean;
  blocking_reasons: string[];
  expires_at: Time;
  initial_window_pt?: Pt;
  plan_revision: string;
  policy_versions: PolicyVersions;
  price_details: {
    cpu_hourly_pt: Pt;
    disk_hourly_pt: Pt;
    gpu_hourly_pt: Pt;
    memory_hourly_pt: Pt;
    total_hourly_pt: Pt;
  };
  reservation: false;
}

export interface PlanInput {
  allowed_overrides: AllowedOverrides;
  allowed_user_actions: CustomAction[];
  code: string;
  gpu: GPURequest;
  gpu_policy: GPUPolicyConfig;
  gpu_user_permissions: Record<string, boolean>;
  image_ids: ID[];
  init_template_version_id?: ID | null;
  instance_type: InstanceType;
  name: string;
  network_option_ids: ID[];
  node_ids: ID[];
  policy_versions: PolicyVersions;
  resources: Resources;
  version: string;
}

export type TemplatePhase = 'cloud_init' | 'post_start';

export interface InitTemplate {
  code: string;
  created_at: Time;
  id: ID;
  name: string;
  parameter_schema: Record<string, unknown>;
  phase: TemplatePhase;
  revision: string;
  script: string;
  status: 'active' | 'draft' | 'retired';
  timeout_seconds: number;
  updated_at: Time;
  version: string;
}

export interface InitTemplateInput {
  code: string;
  name: string;
  parameter_schema: Record<string, unknown>;
  phase: TemplatePhase;
  script: string;
  timeout_seconds: number;
  version: string;
}

export interface GPUOption {
  access_mode: AccessMode;
  availability: 'available' | 'unavailable';
  created_at: Time;
  gpu_hourly_pt: Pt;
  id: ID;
  memory_mib: null | number;
  model: string;
}

export interface NetworkOption {
  created_at: Time;
  id: ID;
  mode: NetworkMode;
  node_id: ID;
  project_name: string;
}

/* ---------- nodes ---------- */

export type AdministrativeState =
  | 'active'
  | 'disabled'
  | 'draining'
  | 'maintenance';
export type NodeHealth = 'offline' | 'online' | 'unknown';
export type ObservationQuality = 'cached' | 'stale' | 'unavailable';

export interface Allocatable {
  disk_bytes: Bytes;
  memory_bytes: Bytes;
  resident_disk_bytes: Bytes;
  resident_memory_bytes: Bytes;
  resident_vcpus: number;
  vcpus: number;
}

export interface NodeConnectionView {
  endpoint: string;
  has_credentials: boolean;
  key_version: number;
  server_cert_sha256: string;
}

export interface NodeConnectionInput {
  endpoint: string;
  server_cert_sha256: string;
  token: string;
}

export interface Node {
  administrative_state: AdministrativeState;
  allocatable: Allocatable;
  connection: NodeConnectionView;
  connection_verified_at: null | Time;
  created_at: Time;
  defaults: { project_name: string; storage_pool: string };
  health: NodeHealth;
  id: ID;
  last_heartbeat_at: null | Time;
  metrics_policy_id: ID;
  name: string;
  resources_sampled_at: null | Time;
  revision: string;
  updated_at: Time;
}

export interface NodeGPUObservation {
  error?: string;
  load_percent: null | number;
  memory_total_mib: null | number;
  memory_used_mib: null | number;
  memory_utilization_percent: null | number;
  model: string;
  pci_address: string;
  utilization_percent: null | number;
  uuid: string;
}

export type GPUStatus =
  | 'available'
  | 'disabled'
  | 'error'
  | 'maintenance'
  | 'missing';
export type GPUHealth = 'error' | 'healthy' | 'missing' | 'unknown';

export interface NodeGPU {
  allocation_count: number;
  created_at: Time;
  health: GPUHealth;
  id: ID;
  last_seen_at: null | Time;
  max_allocations: number;
  memory_mib: null | number;
  model: string;
  node_id: ID;
  observation: NodeGPUObservation | null;
  observed_at: null | Time;
  pci_address: string;
  quality: ObservationQuality;
  reservation_count: number;
  revision: string;
  status: GPUStatus;
  updated_at: Time;
  uuid: string;
}

export interface NodeInterfaceAddress {
  address: string;
  family: string;
  netmask?: string;
  network?: string;
  prefix_length?: number | string;
}

export interface NodeInterface {
  addresses: NodeInterfaceAddress[];
  index: number;
  loopback: boolean;
  mac: string;
  mtu: number;
  name: string;
  up: boolean;
}

export interface NodeResources {
  health: NodeHealth;
  observed_at: null | Time;
  quality: ObservationQuality;
  resources: null | {
    average_load_percent: null | number;
    gpus: NodeGPUObservation[];
    network_interfaces: NodeInterface[];
    server_resources?: {
      cpu?: { architecture?: string; total?: number };
      memory?: { total?: number; used?: number };
      storage?: {
        disks?: Array<{
          id?: string;
          model?: string;
          size?: number;
          type?: string;
        }>;
        total?: number;
      };
      system?: { product?: string; type?: string; vendor?: string };
    };
  };
}

export interface UnmanagedInstance {
  architecture: string;
  config: Record<string, unknown>;
  created_at: Time;
  id: ID;
  incus_name: string;
  incus_uuid: null | string;
  instance_type: InstanceType;
  node_id: ID;
  observed_at: Time;
  observed_state: string;
  project_name: string;
  revision: string;
}

/* ---------- network ---------- */

export type NetworkMode = 'bridge' | 'macvlan' | 'managed_bridge';

export interface NetworkPool {
  address_family: number;
  cidr: string;
  created_at: Time;
  dns_servers: string[];
  gateway: null | string;
  id: ID;
  name: string;
  range_end: string;
  range_start: string;
  revision: string;
  status: 'active' | 'disabled' | 'exhausted';
  updated_at: Time;
}

export type AddressStatus =
  | 'available'
  | 'bound'
  | 'conflict'
  | 'quarantined'
  | 'reserved';

export interface IPAddress {
  address: string;
  address_family: number;
  created_at: Time;
  id: ID;
  instance_id: ID | null;
  network_pool_id: ID;
  node_id: ID | null;
  node_network_binding_id: ID | null;
  reservation_task_id: ID | null;
  revision: string;
  status: AddressStatus;
}

export interface NetworkBinding {
  created_at: Time;
  id: ID;
  incus_network_name: null | string;
  mode: NetworkMode;
  mtu: null | number;
  network_pool_id: ID;
  node_id: ID;
  parent_interface: null | string;
  project_name: string;
  revision: string;
  status: 'active' | 'disabled';
  validated_at: null | Time;
  validation_details: null | Record<string, unknown>;
  validation_status: 'failed' | 'pending' | 'verified';
}

/* ---------- instances ---------- */

export type ObservedState =
  | 'deleted'
  | 'error'
  | 'frozen'
  | 'pending'
  | 'running'
  | 'stopped'
  | 'unknown';
export type ProvisioningState =
  | 'creating'
  | 'deleted'
  | 'deleting'
  | 'failed'
  | 'initializing'
  | 'pending'
  | 'ready'
  | 'unknown';

export interface Capabilities {
  console: boolean;
  delete: boolean;
  /** Explicit member cleanup permission; older/absent responses fail closed. */
  force_delete?: boolean;
  gpu_attach: boolean;
  gpu_rebind: boolean;
  resize: boolean;
  start: boolean;
  stop: boolean;
}

export interface PlanSnapshot extends Resources {
  allowed_overrides: AllowedOverrides;
  revision: string;
}

export interface Instance {
  active_task_id: ID | null;
  /** Actual selected task status; an active cancelled task remains a safety barrier pending reconciliation. */
  active_task_status?: null | TaskStatus;
  creation_task_id?: ID | null;
  initialization_pending?: boolean;
  login_mode?:
    | 'none'
    | 'password'
    | 'password_and_ssh_key'
    | 'ssh_key'
    | 'unknown';
  billing_mode: 'admin_exempt' | 'credit';
  capabilities: Capabilities;
  created_at: Time;
  credit_enforcement: {
    event_id: ID | null;
    shutdown_deadline: null | Time;
    status:
      | 'cancelled'
      | 'completed'
      | 'exempt'
      | 'grace'
      | 'normal'
      | 'stopping'
      | 'unknown';
  };
  desired_state: 'deleted' | 'running' | 'stopped';
  id: ID;
  image_id: ID;
  image_os_name: string;
  image_os_release: string;
  instance_type: InstanceType;
  name: string;
  node_id: ID;
  observed_at: null | Time;
  observed_state: ObservedState;
  plan_id: ID | null;
  plan_snapshot: null | PlanSnapshot;
  policy_versions: PolicyVersions;
  product_type: 'custom' | 'elastic';
  provisioning_state: ProvisioningState;
  /** 仅后端确认创建未派发且原子释放预留后为 true；缺失不视为释放。 */
  resource_reservations_released?: boolean;
  resources: Resources;
  revision: string;
  state_stale: boolean;
  updated_at: Time;
  user_id: ID;
}

export interface InstanceNetwork {
  dns: string[];
  gateway: string;
  ip: string;
  mode: NetworkMode;
  observed_at: null | Time;
  prefix_length: number;
  quality: { configuration_bound: boolean; state_stale: boolean };
  status: string;
}

export type GPUAllocationStatus =
  | 'active'
  | 'attaching'
  | 'cancelled'
  | 'failed'
  | 'released'
  | 'releasing'
  | 'reserved'
  | 'unknown';

export interface GPUAllocation {
  configured: boolean | null;
  created_at: Time;
  gpu_hourly_pt: Pt;
  gpu_price_version_id: ID | null;
  id: ID;
  model: string;
  observed: boolean | null;
  release_verified: boolean;
  released_at: null | Time;
  revision: string;
  status: GPUAllocationStatus;
  uuid: string;
}

export interface GPUPolicyConfig {
  idle: {
    action: 'detach_gpu' | 'stop_instance';
    enabled: boolean;
    timeout_seconds: number;
  };
  rebalance: { allow_live: boolean; enabled: boolean; max_hourly_pt: Pt };
}

export interface GPUPolicy extends GPUPolicyConfig {
  gpu_binding: null | {
    policy_version_id: ID;
    model: string;
    access_mode: AccessMode;
    gpu_hourly_pt: Pt;
  };
  allowed_ranges: {
    editable: Record<
      | 'allow_live'
      | 'idle'
      | 'idle_action'
      | 'idle_timeout'
      | 'max_hourly_pt'
      | 'rebalance',
      boolean
    >;
    idle: {
      allowed_actions: Array<'detach_gpu' | 'stop_instance'>;
      default_timeout_seconds: number;
      enabled: boolean;
      max_timeout_seconds: number;
      min_timeout_seconds: number;
    };
    rebalance: {
      allow_price_increase: boolean;
      max_hourly_pt: Pt;
      min_hourly_pt: Pt;
    };
  };
  allowed_user_actions: string[];
  blocked_reasons: string[];
  effective_policy: GPUPolicyConfig & {
    safety: Record<string, boolean | number>;
  };
  gpu_user_permissions: Record<string, boolean>;
  idle_since: null | Time;
  last_rebound_at: null | Time;
  policy_versions: PolicyVersions;
  revision: string;
}

export interface MetricQuality {
  attribution_complete: boolean;
  complete: boolean;
  error_code?: null | string;
  reasons: string[];
  reference_scan_quality?: string;
}

export interface LatestGPUMetric {
  configured: boolean;
  memory_total_bytes?: Bytes | null;
  model?: string;
  observed: boolean;
  process_count: null | number;
  quality: MetricQuality;
  referencing_process_count: null | number;
  sm_utilization_percent: null | number;
  state: string;
  used_memory_bytes: Bytes | null;
  uuid: string;
}

export interface LatestMetrics {
  age_seconds: string;
  collection_complete: boolean;
  cpu: { allocation_percent: null | number; window_seconds: null | number };
  gpu_state: string;
  gpus: LatestGPUMetric[];
  instance_id: ID;
  instance_state: string;
  memory: {
    limit_bytes: Bytes | null;
    peak_bytes: Bytes | null;
    used_bytes: Bytes | null;
  };
  quality: MetricQuality;
  sampled_at: Time;
  stale: boolean;
}

export interface HistoryGPUPoint {
  gpu_memory_total_bytes?: Bytes | null;
  gpu_model?: string;
  gpu_uuid?: string;
  memory_peak_bytes?: Bytes | null;
  memory_used_avg_bytes?: Bytes | null;
  // raw points use the latest-metrics field names
  sm_utilization_percent?: null | number;
  used_memory_bytes?: Bytes | null;
  utilization_avg_pct?: null | number | string;
  utilization_peak_pct?: null | number | string;
  uuid?: string;
}

export interface HistoryPoint {
  bucket_start_at?: Time;
  cpu?: { allocation_percent: null | number };
  cpu_utilization_avg_pct?: null | number | string;
  cpu_utilization_peak_pct?: null | number | string;
  gpus: HistoryGPUPoint[];
  memory?: { used_bytes: Bytes | null };
  memory_peak_bytes?: Bytes | null;
  memory_used_avg_bytes?: Bytes | null;
  sampled_at?: Time;
}

export interface MetricsHistory {
  available_range: unknown;
  bucket_seconds: number;
  collection_gaps: Array<{ from_at: Time; reason: string; to_at: Time }>;
  coverage: { fraction: string; seconds: string };
  from: Time;
  gaps: Array<{ from: Time; reason: string; to: Time }>;
  items: HistoryPoint[];
  next_cursor: null | string;
  resolution: 'day' | 'hour' | 'minute' | 'raw';
  to: Time;
}

export type ConsoleKind = 'serial' | 'terminal' | 'vga';
export type ConsoleStatus =
  | 'active'
  | 'closed'
  | 'expired'
  | 'failed'
  | 'preparing'
  | 'ready';

export interface ConsoleSession {
  kind: ConsoleKind;
  protocol: '101cloud.terminal.v1' | '101cloud.vnc.v1';
  revision: string;
  session_id: string;
  status: ConsoleStatus;
  websocket_path: string;
}

/* ---------- tasks ---------- */

export type TaskStatus =
  | 'cancelled'
  | 'failed'
  | 'queued'
  | 'running'
  | 'succeeded'
  | 'unknown'
  | 'waiting';

export interface TaskStep {
  error?: null | PublicError;
  error_code: null | string;
  finished_at: null | Time;
  name: string;
  started_at: null | Time;
  status: TaskStatus;
}

export type CreationStageKey =
  | 'preflight'
  | 'native'
  | 'gpu'
  | 'start'
  | 'cloud_init'
  | 'script'
  | 'verify';
export type CreationStageStatus =
  | 'pending'
  | 'running'
  | 'succeeded'
  | 'skipped'
  | 'failed'
  | 'unknown';

export interface CreationStage {
  key: CreationStageKey;
  status: CreationStageStatus;
  started_at?: null | Time;
  finished_at?: null | Time;
}

export interface CreationProgress {
  stages: CreationStage[];
  initialization_pending: boolean;
}

export interface CreationCleanup {
  status: 'pending' | 'cleaning' | 'unknown' | 'completed';
  billing_status: 'pending' | 'refunded';
  instance_name: string;
}

export interface Task {
  can_force_stop?: boolean;
  force_stop_blocked_reason?: string;
  creation_cleanup?: CreationCleanup | null;
  creation_progress?: CreationProgress | null;
  error?: null | PublicError;
  created_at: Time;
  error_code: null | string;
  finished_at: null | Time;
  force_stop?: null | {
    orchestration_stopped: true;
    external_state: 'not_dispatched' | 'requires_reconciliation';
    requires_reconciliation: boolean;
    resources_retained?: boolean;
    reconciliation_completed?: boolean;
  };
  id: ID;
  instance_id: ID | null;
  next_action: string;
  node_id: ID | null;
  phase: null | string;
  resource_reservations_released?: boolean;
  result?: null | Record<string, unknown>;
  revision: string;
  started_at: null | Time;
  status: TaskStatus;
  steps: TaskStep[];
  type: string;
  user_id: ID | null;
}

/* ---------- requests ---------- */

export type CustomAction =
  | 'console'
  | 'delete'
  | 'gpu_attach'
  | 'gpu_detach'
  | 'gpu_policy'
  | 'gpu_rebind'
  | 'resize'
  | 'restart'
  | 'start'
  | 'stop';

export const CUSTOM_ACTIONS: CustomAction[] = [
  'start',
  'stop',
  'restart',
  'resize',
  'delete',
  'gpu_attach',
  'gpu_detach',
  'gpu_rebind',
  'gpu_policy',
  'console',
];

export const GPU_USER_PERMISSIONS = [
  'idle',
  'idle_timeout',
  'idle_action',
  'rebalance',
  'allow_live',
  'max_hourly_pt',
] as const;

export interface CustomConfig {
  allowed_user_actions?: CustomAction[];
  gpu: GPURequest & { fixed_uuids?: string[] };
  gpu_user_permissions?: Record<string, boolean>;
  image_id: ID;
  init_parameters: Record<string, unknown>;
  init_template_version_id?: ID;
  instance_type: InstanceType;
  network_option_id: ID;
  node_id: ID;
  resources: Resources;
}

/* ---------- policies & finance ---------- */

export interface PolicyVersion {
  code: string;
  config: Record<string, unknown>;
  created_at: Time;
  created_by_user_id?: ID;
  effective_at: null | Time;
  id: ID;
  kind: PolicyKind;
  revision: string;
  status: 'active' | 'draft' | 'retired';
  updated_at: Time;
  version: string;
}

export interface CreditEnforcement {
  account_id?: ID;
  created_at?: Time;
  exhausted_at: Time;
  id: ID;
  instance_ids: ID[];
  instances?: Array<Record<string, unknown>>;
  notification?: { created_at: Time; id: ID; read_at: null | Time };
  policy_version_id: ID;
  revision: string;
  shutdown_deadline: Time;
  status: 'cancelled' | 'completed' | 'grace' | 'stopping' | 'unknown';
  user_id: ID;
}

export interface CreditAccount {
  available_pt: Pt;
  enforcements: CreditEnforcement[];
  id: ID;
  next_grant_at: Time;
  outstanding_debt_pt: Pt;
  policy_version_id: ID;
  remaining_pt: Pt;
  reserved_pt: Pt;
  revision: string;
  server_time: Time;
  settlement_pending: boolean;
  status: 'active' | 'closed' | 'frozen';
  user_id: ID;
}

export interface CreditAdjustmentResult {
  account_id: ID;
  amount_pt: Pt;
  available_pt: Pt;
  bucket_id: ID | null;
  expires_at: null | Time;
  outstanding_debt_pt: Pt;
  remaining_pt: Pt;
  reserved_pt: Pt;
  revision: string;
  settled_debt_pt: Pt;
  settlement_pending: boolean;
}

export interface LedgerEntry {
  account_id: ID;
  amount_delta_pt: Pt;
  bucket_id: ID;
  created_at: Time;
  entry_type:
    | 'adjust'
    | 'expire'
    | 'grant'
    | 'refund'
    | 'release'
    | 'reserve'
    | 'settle';
  id: ID;
  remaining_after_pt: Pt;
  reserved_after_pt: Pt;
  reserved_delta_pt: Pt;
}

export interface RedemptionCode {
  code?: string;
  created_at: Time;
  display_prefix: string;
  expires_at: Time;
  grant_pt: Pt;
  id: ID;
  max_uses: number;
  revision: string;
  status: 'active' | 'disabled';
  used_count: number;
  valid_from: Time;
}

export interface AuditLog {
  actor_user_id: ID | null;
  created_at: Time;
  event_type: string;
  id: ID;
  occurred_at: Time;
  request_id: null | string;
  resource_id: ID | null;
  resource_type: string;
  result: string;
  task_id: ID | null;
}
