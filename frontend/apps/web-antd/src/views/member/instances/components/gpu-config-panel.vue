<script lang="ts" setup>
import type { AttachBody } from '#/api/backend';
import type {
  GPUAllocation,
  GPUPolicy,
  GPUPolicyConfig,
  Instance,
  LatestMetrics,
} from '#/api/types';

import { computed, onBeforeUnmount, ref, shallowRef, watch } from 'vue';
import { useAccessStore } from '@vben/stores';
import {
  Alert,
  Button,
  Card,
  Collapse,
  CollapsePanel,
  Descriptions,
  DescriptionsItem,
  Empty,
  Form,
  FormItem,
  Input,
  InputNumber,
  Select,
  Spin,
  Switch,
  Table,
  Tag,
} from 'ant-design-vue';

import { memberApi } from '#/api/backend';
import { ApiError, newIdempotencyKey } from '#/api/request';
import ConfirmAction from '#/components/confirm-action.vue';
import ErrorFeedback from '#/components/error-feedback.vue';
import TimeText from '#/components/time-text.vue';
import { trackAccepted, useAction, usePolling } from '#/composables/api';
import { enumLabel, QUALITY_REASONS } from '#/locales/enums';
import { describeError } from '#/locales/errors';
import { useAuthStore } from '#/store/auth';
import {
  formatBytes,
  formatPercent,
  formatPt,
  PT_PATTERN,
  ptToMicros,
  shortId,
} from '#/utils/format';
import { gpuAttachmentState } from '#/utils/gpu-attachment';

const props = defineProps<{
  instance: Instance;
  etag: string | null;
  active: boolean;
  disabled?: boolean;
}>();
const emit = defineEmits<{ changed: []; unresolved: [value: boolean]; submitting: [value: boolean] }>();
const auth = useAuthStore();
const access = useAccessStore();
const actor = computed(() =>
  auth.me?.user.role === 'user' && !access.loginExpired
    ? auth.me.user.id
    : null,
);
const ownsInstance = computed(() =>
  Boolean(actor.value && actor.value === props.instance.user_id),
);
let mounted = true;
let epoch = 0;
let allocationSequence = 0;
let policySequence = 0;
let metricSequence = 0;
const allocations = shallowRef<GPUAllocation[]>([]);
const policy = shallowRef<GPUPolicy | null>(null);
const latest = shallowRef<LatestMetrics | null>(null);
const policyEtag = ref<string | null>(null);
const allocationLoading = ref(false);
const allocationLoaded = ref(false);
const policyLoading = ref(false);
const allocationError = shallowRef<ReturnType<typeof describeError> | null>(
  null,
);
const policyError = shallowRef<ReturnType<typeof describeError> | null>(null);
const metricError = shallowRef<ReturnType<typeof describeError> | null>(null);
const actionError = shallowRef<ReturnType<typeof describeError> | null>(null);
const selected = ref<string[]>([]);
const showHistory = ref(false);
const draft = ref<GPUPolicyConfig | null>(null);
const draftBase = shallowRef<GPUPolicyConfig | null>(null);
const draftEtag = ref<string | null>(null);
const conflict = ref(false);
const notice = ref('');
const action = useAction();
watch(action.pending, (value) => emit('submitting', value), { flush: 'sync' });
const uncertain = ref(false);
const acceptedTask = ref<string | null>(null);
const dialogOpen = ref(false);
const dialogKind = ref<
  'attach' | 'detach' | 'rebind' | 'policy' | 'review' | 'discard'
>('attach');
const dialogRevision = ref('');
const count = ref<number | undefined>(1);
const maxHourly = ref('');
const binding = computed(() => policy.value?.gpu_binding ?? null);
const cloneConfig = (value: GPUPolicyConfig): GPUPolicyConfig => ({
  idle: { ...value.idle },
  rebalance: { ...value.rebalance },
});
const dirty = computed(() =>
  Boolean(
    draft.value &&
    draftBase.value &&
    JSON.stringify(draft.value) !== JSON.stringify(draftBase.value),
  ),
);
const requiresReview = computed(
  () => dirty.value && draftEtag.value !== policyEtag.value,
);
const running = computed(() => props.instance.observed_state === 'running');
const allowed = (name: string) =>
  policy.value?.allowed_user_actions.includes(name) === true;
type Mutation =
  | { kind: 'attach'; body: Omit<AttachBody, 'fixed_uuids'> }
  | { kind: 'detach'; body: { allocation_ids: string[]; allow_live: boolean } }
  | {
      kind: 'rebind';
      body: {
        allocation_ids: string[];
        allow_live: boolean;
        max_hourly_pt: string;
      };
    }
  | { kind: 'policy'; body: GPUPolicyConfig };
type Intent = Mutation & {
  id: string;
  actor: string;
  epoch: number;
  etag: string;
  key: string;
};
const intent = shallowRef<Intent | null>(null);
watch(
  intent,
  (value) => emit('unresolved', Boolean(value && !acceptedTask.value)),
  { flush: 'sync' },
);
watch(
  acceptedTask,
  (value) => {
    if (value) emit('unresolved', false);
  },
  { flush: 'sync' },
);
const locked = computed(() =>
  Boolean(intent.value || acceptedTask.value || action.pending.value),
);
const current = (id: string, token: number) =>
  mounted && ownsInstance.value && props.instance.id === id && epoch === token;
const activeSelected = computed(() =>
  allocations.value.filter(
    (item) => selected.value.includes(item.id) && item.status === 'active',
  ),
);
// A released record without independent verification is still pending confirmation.
const history = (item: GPUAllocation) =>
  item.status === 'cancelled' ||
  item.status === 'failed' ||
  (item.status === 'released' && item.release_verified);
const rows = computed(() =>
  allocations.value.filter((item) => showHistory.value || !history(item)),
);
const baseProblem = computed(() => {
  if (!ownsInstance.value)
    return '仅实例所属普通用户可以操作；请核对当前登录账号。';
  if (props.disabled) return '其他实例请求结果尚未确认，请先核对。';
  if (props.instance.instance_type !== 'container')
    return 'GPU 配置仅支持容器实例。';
  if (props.instance.active_task_id)
    return 'GPU 变更已暂停，请先核对上方任务。';
  if (acceptedTask.value)
    return 'GPU 操作已受理，等待任务及独立观测；不要重复操作。';
  if (
    props.instance.provisioning_state !== 'ready' ||
    props.instance.initialization_pending
  )
    return '实例尚未就绪。';
  if (props.instance.state_stale)
    return '实例观测已过期，请刷新并等待有效观测。';
  if (!['running', 'stopped'].includes(props.instance.observed_state))
    return '实例运行状态尚未确认。';
  if (!props.etag) return '实例版本信息缺失，请刷新。';
  if (policyError.value || !policy.value)
    return 'GPU 策略尚未成功加载，请刷新。';
  return '';
});
const creditBlocked = computed(() =>
  ['grace', 'stopping', 'unknown'].includes(
    props.instance.credit_enforcement.status,
  ),
);
const creditProblem = computed(() =>
  creditBlocked.value
    ? '存在额度停机事件；挂载、换卡及策略变更暂不可用，已授权的安全卸载仍可执行。'
    : '',
);
const allocationProblem = computed(() =>
  !allocationLoaded.value || allocationError.value
    ? 'GPU 分配状态尚未确认，请刷新分配。'
    : '',
);
const canAttach = computed(
  () =>
    !locked.value &&
    !baseProblem.value &&
    !creditProblem.value &&
    !allocationProblem.value &&
    props.instance.capabilities.gpu_attach &&
    allowed('gpu_attach') &&
    Boolean(binding.value),
);
const releaseSafe = (item: GPUAllocation) => {
  const metric = metricsFor(item);
  if (!running.value) return true;
  const data = latest.value;
  return Boolean(
    data &&
    !metricError.value &&
    !data.stale &&
    data.instance_state === 'running' &&
    data.collection_complete &&
    data.quality.complete &&
    data.quality.attribution_complete &&
    data.quality.reference_scan_quality === 'complete' &&
    metric?.quality.complete &&
    metric.quality.attribution_complete &&
    metric.quality.reference_scan_quality === 'complete' &&
    metric.state === 'idle_candidate' &&
    metric.process_count === 0 &&
    metric.referencing_process_count === 0 &&
    metric.used_memory_bytes != null &&
    /^0+$/.test(metric.used_memory_bytes),
  );
};
const selectedReleaseSafe = computed(
  () =>
    activeSelected.value.length > 0 && activeSelected.value.every(releaseSafe),
);
const canDetach = computed(
  () =>
    !locked.value &&
    !baseProblem.value &&
    !allocationProblem.value &&
    selectedReleaseSafe.value &&
    allowed('gpu_detach') &&
    activeSelected.value.length > 0,
);
const canRebind = computed(
  () =>
    !locked.value &&
    !baseProblem.value &&
    !creditProblem.value &&
    !allocationProblem.value &&
    props.instance.capabilities.gpu_rebind &&
    allowed('gpu_rebind') &&
    activeSelected.value.length > 0 &&
    Boolean(binding.value),
);
const canPolicy = computed(
  () =>
    !locked.value &&
    !baseProblem.value &&
    !creditProblem.value &&
    allowed('gpu_policy') &&
    Boolean(policyEtag.value),
);
const fieldEditable = (field: keyof GPUPolicy['allowed_ranges']['editable']) =>
  allowed('gpu_policy') &&
  policy.value?.allowed_ranges.editable[field] === true;
const fieldDisabled = (field: keyof GPUPolicy['allowed_ranges']['editable']) =>
  locked.value || !fieldEditable(field);
const readOnlyHint = (field: keyof GPUPolicy['allowed_ranges']['editable']) =>
  fieldEditable(field) ? '' : '管理员未授权修改此字段（只读，保存时原样保留）';

async function loadAllocations() {
  if (!ownsInstance.value) return;
  const id = props.instance.id;
  const token = epoch;
  const sequence = ++allocationSequence;
  allocationLoading.value = true;
  try {
    const response = await memberApi.gpuAllocations(id);
    if (!current(id, token) || sequence !== allocationSequence) return;
    allocations.value = response.data;
    allocationLoaded.value = true;
    allocationError.value = null;
    if (!intent.value)
      selected.value = selected.value.filter((key) =>
        response.data.some(
          (item) => item.id === key && item.status === 'active',
        ),
      );
  } catch (error) {
    if (current(id, token) && sequence === allocationSequence)
      allocationError.value = describeError(error);
  } finally {
    if (current(id, token) && sequence === allocationSequence)
      allocationLoading.value = false;
  }
}
async function loadLatest() {
  if (!ownsInstance.value) return;
  const id = props.instance.id;
  const token = epoch;
  const sequence = ++metricSequence;
  try {
    const response = await memberApi.latest(id);
    if (!current(id, token) || sequence !== metricSequence) return;
    latest.value = response.data;
    metricError.value = null;
  } catch (error) {
    if (current(id, token) && sequence === metricSequence) {
      latest.value = null;
      metricError.value = describeError(error);
    }
  }
}
function fillDraft(value: GPUPolicyConfig, etag: string | null) {
  draft.value = cloneConfig(value);
  draftBase.value = cloneConfig(value);
  draftEtag.value = etag;
}
function mergeDraft(
  current: GPUPolicy,
  original: GPUPolicyConfig,
  changes: GPUPolicyConfig,
) {
  const result = cloneConfig(current);
  const editable = current.allowed_ranges.editable;
  if (editable.idle && changes.idle.enabled !== original.idle.enabled)
    result.idle.enabled = changes.idle.enabled;
  if (
    editable.idle_timeout &&
    changes.idle.timeout_seconds !== original.idle.timeout_seconds
  )
    result.idle.timeout_seconds = changes.idle.timeout_seconds;
  if (editable.idle_action && changes.idle.action !== original.idle.action)
    result.idle.action = changes.idle.action;
  if (
    editable.rebalance &&
    changes.rebalance.enabled !== original.rebalance.enabled
  )
    result.rebalance.enabled = changes.rebalance.enabled;
  if (
    editable.allow_live &&
    changes.rebalance.allow_live !== original.rebalance.allow_live
  )
    result.rebalance.allow_live = changes.rebalance.allow_live;
  if (
    editable.max_hourly_pt &&
    changes.rebalance.max_hourly_pt !== original.rebalance.max_hourly_pt
  )
    result.rebalance.max_hourly_pt = changes.rebalance.max_hourly_pt;
  return result;
}
async function loadPolicy() {
  if (!ownsInstance.value) return;
  const id = props.instance.id;
  const token = epoch;
  const sequence = ++policySequence;
  policyLoading.value = true;
  try {
    const response = await memberApi.gpuPolicy(id);
    if (!current(id, token) || sequence !== policySequence) return;
    const preserve = dirty.value || intent.value?.kind === 'policy';
    if (
      preserve &&
      !intent.value &&
      policy.value &&
      draftBase.value &&
      draft.value
    )
      draft.value = mergeDraft(response.data, draftBase.value, draft.value);
    policy.value = response.data;
    policyEtag.value = response.etag;
    policyError.value = null;
    if (!preserve) fillDraft(response.data, response.etag);
    if (preserve && draftEtag.value !== response.etag)
      notice.value =
        '已取得最新策略；可编辑的本地修改已保留，未修改字段采用最新配置。请核对生效配置和 ETag 后再保存。';
    if (!intent.value) conflict.value = false;
  } catch (error) {
    if (current(id, token) && sequence === policySequence)
      policyError.value = describeError(error);
  } finally {
    if (current(id, token) && sequence === policySequence)
      policyLoading.value = false;
  }
}
async function refresh() {
  await Promise.all([loadAllocations(), loadPolicy(), loadLatest()]);
}
const priceProblem = (value: string) => {
  const amount = ptToMicros(value);
  return !PT_PATTERN.test(value) ||
    amount === null ||
    amount > 99_999_999_999_999_999_999n
    ? '价格须为 0–99999999999999.999999 pt，最多 6 位小数；0 不是不限价。'
    : '';
};
const policyProblem = computed(() => {
  const value = draft.value;
  const source = policy.value;
  if (!value || !source) return '策略尚未加载。';
  if (requiresReview.value || conflict.value)
    return '请刷新、核对最新策略版本后再保存；不会自动覆盖。';
  const editable = source.allowed_ranges.editable;
  if (
    (!editable.idle && value.idle.enabled !== source.idle.enabled) ||
    (!editable.idle_timeout &&
      value.idle.timeout_seconds !== source.idle.timeout_seconds) ||
    (!editable.idle_action && value.idle.action !== source.idle.action) ||
    (!editable.rebalance &&
      value.rebalance.enabled !== source.rebalance.enabled) ||
    (!editable.allow_live &&
      value.rebalance.allow_live !== source.rebalance.allow_live) ||
    (!editable.max_hourly_pt &&
      value.rebalance.max_hourly_pt !== source.rebalance.max_hourly_pt)
  )
    return '草稿包含当前账号无权修改的字段；请核对并放弃这些修改。';
  const range = source.allowed_ranges;
  if (
    !Number.isInteger(value.idle.timeout_seconds) ||
    value.idle.timeout_seconds < range.idle.min_timeout_seconds ||
    value.idle.timeout_seconds > range.idle.max_timeout_seconds
  )
    return `空闲时长须在 ${range.idle.min_timeout_seconds}–${range.idle.max_timeout_seconds} 秒之间。`;
  if (!range.idle.allowed_actions.includes(value.idle.action))
    return '空闲动作不在允许范围内。';
  if (value.idle.enabled && !range.idle.enabled)
    return '实例绑定的政策未启用空闲回收。';
  if (priceProblem(value.rebalance.max_hourly_pt))
    return priceProblem(value.rebalance.max_hourly_pt);
  const amount = ptToMicros(value.rebalance.max_hourly_pt)!;
  const low = ptToMicros(range.rebalance.min_hourly_pt);
  const high = ptToMicros(range.rebalance.max_hourly_pt);
  if (low === null || high === null || amount < low || amount > high)
    return `价格上限须在 ${formatPt(range.rebalance.min_hourly_pt)}–${formatPt(range.rebalance.max_hourly_pt)} pt/h 之间。`;
  return '';
});
const dialogProblem = computed(() => {
  if (intent.value)
    return '该请求已有未确认结果；请关闭弹窗后使用“按原请求重试”，不要改写请求。';
  if (dialogKind.value === 'review' || dialogKind.value === 'discard')
    return locked.value ? '请求未确认时不能修改草稿。' : '';
  if (dialogRevision.value !== props.instance.revision)
    return '确认期间实例版本发生变化，请关闭并刷新核对后重新确认。';
  if (dialogKind.value === 'policy')
    return !canPolicy.value
      ? baseProblem.value || creditProblem.value || '未授权或正在操作。'
      : policyProblem.value;
  if (baseProblem.value || allocationProblem.value)
    return baseProblem.value || allocationProblem.value;
  if (dialogKind.value === 'detach')
    return canDetach.value
      ? ''
      : !selectedReleaseSafe.value
        ? '运行中的卸载需要完整、新鲜的归因观测，并确认 GPU 进程、设备引用和显存均为空闲；缺失或过期观测时请等待刷新。'
        : '当前账号没有卸载权限。';
  if (creditProblem.value) return creditProblem.value;
  if (!binding.value) return '实例没有 GPU 定价绑定，请联系管理员。';
  if (dialogKind.value === 'attach') {
    if (!canAttach.value) return '实例未授权挂载 GPU。';
    if (
      count.value === undefined ||
      !Number.isSafeInteger(count.value) ||
      count.value < 1 ||
      count.value > 64
    )
      return '数量须为 1–64 的整数。';
  } else if (!canRebind.value) return '请选择有效分配，且须有换卡权限。';
  if (
    dialogKind.value === 'rebind' &&
    activeSelected.value.some((item) => item.model !== binding.value?.model)
  )
    return '所选分配型号与绑定型号不同，请联系管理员核对。';
  if (priceProblem(maxHourly.value)) return priceProblem(maxHourly.value);
  const amount = ptToMicros(maxHourly.value);
  const bound = ptToMicros(binding.value.gpu_hourly_pt);
  if (amount === null || bound === null || amount < bound)
    return `单卡上限不能低于绑定单价 ${formatPt(binding.value.gpu_hourly_pt)} pt/h。`;
  return '';
});
const policyRisk = computed(() => {
  const value = draft.value;
  if (!value) return '';
  return [
    value.idle.action === 'stop_instance'
      ? '我确认所选空闲动作会停止实例（仅在启用空闲回收时生效）'
      : '',
    value.rebalance.allow_live
      ? '我明确授权运行中换卡；CUDA 上下文不会迁移，且仍须通过平台安全检查'
      : '',
  ]
    .filter(Boolean)
    .join('；');
});
const dialogTitle = computed(
  () =>
    ({
      attach: '确认挂载 GPU',
      detach: '确认卸载 GPU',
      rebind: '确认换卡',
      policy: '确认保存 GPU 策略',
      review: '核对最新版本并保留草稿',
      discard: '放弃本地策略修改',
    })[dialogKind.value],
);
const dialogRisk = computed(() => {
  const request = intent.value;
  const allowLive =
    request?.kind === 'attach' ||
    request?.kind === 'detach' ||
    request?.kind === 'rebind'
      ? request.body.allow_live
      : running.value;
  return ['attach', 'detach', 'rebind'].includes(dialogKind.value)
    ? allowLive
      ? '我明确允许本次请求 allow_live=true；CUDA 上下文不会迁移'
      : '我确认按 allow_live=false 提交本次 GPU 变更'
    : dialogKind.value === 'policy'
      ? policyRisk.value
      : '我已对照最新配置核对本次更改';
});
function openDialog(kind: typeof dialogKind.value) {
  if (locked.value) return;
  if (
    (kind === 'attach' && !canAttach.value) ||
    (kind === 'detach' && !canDetach.value) ||
    (kind === 'rebind' && !canRebind.value) ||
    (kind === 'policy' && (!canPolicy.value || policyProblem.value))
  )
    return;
  dialogKind.value = kind;
  dialogRevision.value = props.instance.revision;
  count.value = 1;
  maxHourly.value = binding.value?.gpu_hourly_pt ?? '';
  dialogOpen.value = true;
}
function isUncertain(error: unknown, retrying: boolean) {
  if (!(error instanceof ApiError)) return true;
  return (
    error.status === 0 ||
    error.status === 408 ||
    error.status >= 500 ||
    error.code === 'external_result_unknown' ||
    (retrying &&
      (error.status === 401 || error.status === 403 || error.status === 429))
  );
}
async function sendIntent() {
  const request = intent.value;
  if (
    !request ||
    props.disabled ||
    action.pending.value ||
    acceptedTask.value ||
    !current(request.id, request.epoch) ||
    request.actor !== actor.value
  )
    return false;
  const retrying = uncertain.value;
  actionError.value = null;
  const result = await action.run(async () => {
    try {
      const write = { ifMatch: request.etag, key: request.key };
      if (request.kind === 'policy')
        return {
          kind: 'policy' as const,
          response: await memberApi.putGPUPolicy(
            request.id,
            request.body,
            write,
          ),
        };
      const response =
        request.kind === 'attach'
          ? await memberApi.attachGPU(request.id, request.body, write)
          : request.kind === 'detach'
            ? await memberApi.detachGPU(request.id, request.body, write)
            : await memberApi.rebindGPU(request.id, request.body, write);
      if (!response.data?.task_id)
        throw new ApiError(
          502,
          {
            code: 'invalid_response',
            details: null,
            message: 'GPU 任务响应不完整，结果待核对',
          },
          response.requestId,
          null,
        );
      return { kind: 'gpu' as const, response };
    } catch (error) {
      if (!current(request.id, request.epoch)) return null;
      actionError.value = describeError(error);
      uncertain.value = isUncertain(error, retrying);
      if (error instanceof ApiError && [409, 412].includes(error.status)) {
        conflict.value = true;
        notice.value =
          '请求版本或配置冲突，请刷新核对；不会自动重新提交或覆盖策略。';
        emit('changed');
      }
      if (!uncertain.value) intent.value = null;
      throw error;
    }
  });
  if (!current(request.id, request.epoch)) return false;
  if (!result) {
    if (!uncertain.value) {
      intent.value = null;
      action.resetKey();
    }
    return false;
  }
  uncertain.value = false;
  if (result.kind === 'policy') {
    intent.value = null;
    policy.value = result.response.data;
    policyEtag.value = result.response.etag;
    fillDraft(result.response.data, result.response.etag);
    notice.value = 'GPU 策略已保存；是否执行以当前生效策略和平台安全规则为准。';
  } else {
    acceptedTask.value = result.response.data.task_id;
    notice.value = '';
    trackAccepted(
      result.response.data,
      `GPU ${request.kind === 'attach' ? '挂载' : request.kind === 'detach' ? '卸载' : '换卡'}`,
      () => {
        if (!current(request.id, request.epoch)) return;
        acceptedTask.value = null;
        intent.value = null;
        selected.value = [];
        void refresh();
        emit('changed');
      },
    );
    void loadAllocations();
  }
  dialogOpen.value = false;
  emit('changed');
  return true;
}
async function confirmDialog() {
  if (dialogProblem.value) return false;
  if (dialogKind.value === 'discard') {
    if (policy.value) fillDraft(policy.value, policyEtag.value);
    return true;
  }
  if (dialogKind.value === 'review') {
    if (!policy.value) return false;
    draft.value = mergeDraft(
      policy.value,
      draftBase.value ?? policy.value,
      draft.value ?? policy.value,
    );
    draftBase.value = cloneConfig(policy.value);
    draftEtag.value = policyEtag.value;
    conflict.value = false;
    notice.value =
      '草稿已按最新服务器配置核对，并保留当前仍获授权的本地字段修改。';
    return true;
  }
  const metadata = {
    actor: actor.value!,
    id: props.instance.id,
    epoch,
    etag: dialogKind.value === 'policy' ? draftEtag.value! : props.etag!,
    key: newIdempotencyKey(),
  };
  if (dialogKind.value === 'policy')
    intent.value = {
      ...metadata,
      etag: draftEtag.value!,
      kind: 'policy',
      body: cloneConfig(draft.value!),
    };
  else if (dialogKind.value === 'attach')
    intent.value = {
      ...metadata,
      kind: 'attach',
      body: {
        model: binding.value!.model,
        access_mode: binding.value!.access_mode,
        count: count.value!,
        max_hourly_pt: maxHourly.value,
        allow_live: running.value,
      },
    };
  else if (dialogKind.value === 'detach')
    intent.value = {
      ...metadata,
      kind: 'detach',
      body: {
        allocation_ids: activeSelected.value.map((item) => item.id),
        allow_live: running.value,
      },
    };
  else
    intent.value = {
      ...metadata,
      kind: 'rebind',
      body: {
        allocation_ids: activeSelected.value.map((item) => item.id),
        max_hourly_pt: maxHourly.value,
        allow_live: running.value,
      },
    };
  return sendIntent();
}
// Display every returned field independently; optional metrics cannot veto known values.
const metricsFor = (item: GPUAllocation) =>
  metricError.value
    ? null
    : (latest.value?.gpus.find((gpu) => gpu.uuid === item.uuid) ?? null);
const usageText = (item: GPUAllocation) => {
  const metric = metricsFor(item);
  if (!metric) return '暂无可用 GPU 采样；占用未知';
  const sm =
    metric.sm_utilization_percent === null &&
    metric.quality.reasons.includes('gpu_utilization_no_samples')
      ? '暂无活动采样'
      : formatPercent(metric.sm_utilization_percent);
  return `SM ${sm} · 归因显存 ${metric.used_memory_bytes == null ? '未知' : formatBytes(metric.used_memory_bytes)} · GPU 进程 ${metric.process_count ?? '未知'} · 设备引用 ${metric.referencing_process_count ?? '未知'}`;
};
const normalReasons = new Set([
  'gpu_utilization_no_samples',
  'instance_not_running',
]);
const metricNotes = (item: GPUAllocation) => {
  const data = latest.value;
  const metric = metricsFor(item);
  if (!data || !metric) return [];
  const reasons = [
    ...new Set(
      [
        ...data.quality.reasons,
        data.quality.error_code,
        ...metric.quality.reasons,
        metric.quality.error_code,
      ].filter((reason): reason is string => Boolean(reason)),
    ),
  ];
  const stopped =
    data.instance_state === 'stopped' &&
    reasons.includes('instance_not_running');
  const knownActivity =
    metric.process_count !== null &&
    metric.referencing_process_count !== null &&
    metric.used_memory_bytes !== null &&
    (metric.sm_utilization_percent !== null ||
      metric.quality.reasons.includes('gpu_utilization_no_samples'));
  const normalOnly =
    reasons.length > 0 &&
    reasons.every((reason) => normalReasons.has(reason)) &&
    data.collection_complete &&
    (stopped ||
      (data.instance_state === 'running' &&
        knownActivity &&
        data.quality.attribution_complete &&
        metric.quality.attribution_complete &&
        data.quality.reference_scan_quality === 'complete' &&
        metric.quality.reference_scan_quality === 'complete' &&
        (!metric.quality.reasons.includes('gpu_utilization_no_samples') ||
          metric.process_count === 0)));
  return [
    {
      text:
        data.instance_state === 'stopped'
          ? '实例已停止，没有运行态 GPU 占用采样；未知不表示占用为 0'
          : '',
      warning: false,
    },
    { text: data.stale ? '采样已过期' : '', warning: true },
    { text: !data.collection_complete ? '采集不完整' : '', warning: true },
    {
      text:
        !normalOnly && (!data.quality.complete || !metric.quality.complete)
          ? '部分指标不可用'
          : '',
      warning: true,
    },
    {
      text:
        !normalOnly &&
        (!data.quality.attribution_complete ||
          !metric.quality.attribution_complete)
          ? '归因不完整'
          : '',
      warning: true,
    },
    {
      text:
        !normalOnly &&
        (data.quality.reference_scan_quality !== 'complete' ||
          metric.quality.reference_scan_quality !== 'complete')
          ? '设备引用扫描未完整确认'
          : '',
      warning: true,
    },
    {
      text: !stopped && !knownActivity ? '部分 GPU 活动指标未知' : '',
      warning: true,
    },
    ...reasons
      .filter((reason) => reason !== 'gpu_utilization_no_samples')
      .map((reason) => ({
        text: QUALITY_REASONS[reason] ?? reason,
        warning: !normalReasons.has(reason),
      })),
  ].filter((note) => note.text);
};
const yesNo = (value: boolean | null) =>
  value === null ? '未知' : value ? '是' : '否';
const releasePending = (item: GPUAllocation) =>
  item.status === 'releasing' ||
  item.status === 'unknown' ||
  (item.status === 'released' && !item.release_verified);
function allocationRecord(value: unknown): GPUAllocation {
  return value as GPUAllocation;
}
const columns = [
  { dataIndex: 'model', title: '型号' },
  { key: 'uuid', title: '短 UUID' },
  { key: 'status', title: '配置状态' },
  { key: 'price', title: '分配单价快照' },
  { key: 'flags', title: '已配置 / 已观测' },
  { key: 'usage', title: '归因活动观测' },
  { key: 'release', title: '释放确认' },
];
const rowSelection = computed(() => ({
  selectedRowKeys: selected.value,
  getCheckboxProps: (item: GPUAllocation) => ({
    disabled: locked.value || item.status !== 'active',
  }),
  onChange: (keys: (string | number)[]) => {
    if (!locked.value) selected.value = keys.map(String);
  },
}));
const idleActions = computed(
  () =>
    policy.value?.allowed_ranges.idle.allowed_actions.map((value) => ({
      value,
      label: value === 'stop_instance' ? '停止实例' : '卸载 GPU',
    })) ?? [],
);
const blockedNames: Record<string, string> = {
  bound_idle_disabled: '实例绑定的空闲政策未开启',
  platform_idle_disabled: '平台未开启空闲回收',
  credit_enforcement_active: '额度停机事件阻止自动均衡',
  gpu_requires_container: '仅支持容器',
  instance_not_ready: '实例尚未就绪',
  idle_timeout_outside_current_range: '空闲时长超出当前范围',
  idle_action_not_allowed: '空闲动作未获允许',
};
const safetyNames: Record<string, string> = {
  allow_force_stop: '允许强制停止',
  allow_price_increase: '允许换到更贵的卡',
  complete_fresh_ownership_required: '要求完整且新鲜的归属证据',
  cooldown_seconds: '冷却时间（秒）',
  device_references_must_be_zero: '设备引用须为零',
  gpu_memory_must_be_zero: '显存占用须为零',
  gpu_processes_must_be_zero: 'GPU 进程须为零',
  graceful_stop_timeout_seconds: '正常停止超时（秒）',
  metrics_max_age_seconds: '指标最大时效（秒）',
};
watch(
  [() => props.instance.id, () => props.instance.user_id, actor],
  () => {
    epoch++;
    allocationSequence++;
    policySequence++;
    metricSequence++;
    allocations.value = [];
    latest.value = null;
    policy.value = null;
    policyEtag.value = null;
    draft.value = null;
    draftBase.value = null;
    draftEtag.value = null;
    selected.value = [];
    allocationLoaded.value = false;
    allocationLoading.value = false;
    policyLoading.value = false;
    allocationError.value = null;
    policyError.value = null;
    metricError.value = null;
    actionError.value = null;
    intent.value = null;
    acceptedTask.value = null;
    uncertain.value = false;
    conflict.value = false;
    notice.value = '';
    dialogOpen.value = false;
    if (!action.pending.value) action.resetKey();
    if (ownsInstance.value) void refresh();
  },
  { immediate: true, flush: 'sync' },
);
watch(
  () => props.instance.revision,
  () => {
    if (ownsInstance.value && props.active) void refresh();
  },
);
usePolling(
  () => (ownsInstance.value && props.active ? refresh() : undefined),
  () => 10_000,
);
onBeforeUnmount(() => {
  mounted = false;
  epoch++;
  allocationSequence++;
  policySequence++;
  metricSequence++;
});
</script>

<template>
  <div class="flex min-w-0 flex-col gap-3">
    <Alert v-if="baseProblem" :message="baseProblem" type="warning" show-icon />
    <Alert
      v-if="creditProblem"
      :message="creditProblem"
      type="warning"
      show-icon
    />
    <Alert v-if="notice" :message="notice" type="info" show-icon />
    <ErrorFeedback v-if="actionError" :feedback="actionError" />
    <Alert
      v-if="intent && !acceptedTask"
      type="warning"
      show-icon
      message="操作结果尚未最终确认"
    >
      <template #description>
        <p>
          原请求内容、If-Match
          与幂等键已锁定，关闭弹窗或刷新不会改写。请核对后手动重试，或联系管理员；不要重复发起新操作。
        </p>
        <Button
          :disabled="!uncertain || action.pending.value || !ownsInstance"
          :loading="action.pending.value"
          size="small"
          @click="sendIntent"
          >按原请求重试</Button
        >
      </template>
    </Alert>
    <Card size="small" title="实例绑定 GPU 定价">
      <Alert
        v-if="policy && !binding"
        type="warning"
        show-icon
        message="实例未绑定 GPU 定价"
        description="请联系管理员完成实例绑定；挂载和换卡不可提交，不从当前套餐或管理员候选推算型号、访问模式及价格。"
      />
      <Descriptions v-else-if="binding" :column="{ xs: 1, sm: 2 }" size="small">
        <DescriptionsItem label="型号">{{ binding.model }}</DescriptionsItem
        ><DescriptionsItem label="访问模式">{{
          binding.access_mode === 'shared' ? '共享' : '独占'
        }}</DescriptionsItem>
        <DescriptionsItem label="绑定单价"
          >{{ formatPt(binding.gpu_hourly_pt) }} pt/卡/小时</DescriptionsItem
        ><DescriptionsItem label="绑定政策版本"
          ><span class="break-all font-mono">{{
            binding.policy_version_id
          }}</span></DescriptionsItem
        >
      </Descriptions>
      <ErrorFeedback :feedback="policyError" />
      <Button
        class="mt-2"
        size="small"
        :loading="policyLoading"
        @click="loadPolicy"
        >刷新策略与绑定</Button
      >
    </Card>
    <Card size="small" title="GPU 分配">
      <div class="mb-3 flex flex-wrap items-center justify-between gap-2">
        <div class="flex flex-wrap gap-2">
          <Button
            type="primary"
            size="small"
            :disabled="!canAttach"
            @click="openDialog('attach')"
            >挂载 GPU</Button
          ><Button
            size="small"
            :disabled="!canDetach"
            @click="openDialog('detach')"
            >卸载所选</Button
          ><Button
            size="small"
            :disabled="!canRebind"
            @click="openDialog('rebind')"
            >换卡</Button
          ><Button
            size="small"
            :loading="allocationLoading"
            @click="loadAllocations"
            >刷新分配</Button
          >
        </div>
        <label class="flex cursor-pointer items-center gap-2 text-sm"
          ><Switch
            v-model:checked="showHistory"
            size="small"
            aria-label="显示历史 GPU 分配"
          />显示历史</label
        >
      </div>
      <p class="text-muted-foreground text-xs">如需卸载显卡，请先停止实例</p>
      <div class="gpu-desktop min-w-0 overflow-x-auto">
        <Table
          :columns="columns"
          :data-source="rows"
          :row-selection="rowSelection"
          row-key="id"
          :pagination="false"
          :scroll="{ x: 920 }"
          :loading="allocationLoading"
          size="small"
        >
          <template #bodyCell="{ column, record }">
            <template v-if="column.key === 'uuid'"
              ><span class="font-mono" :title="record.uuid">{{
                shortId(record.uuid)
              }}</span></template
            >
            <template v-else-if="column.key === 'status'"
              ><Tag class="m-0">{{
                enumLabel(
                  'gpuAttachment',
                  gpuAttachmentState(allocationRecord(record)),
                ).text
              }}</Tag></template
            >
            <template v-else-if="column.key === 'price'"
              >{{
                formatPt(allocationRecord(record).gpu_hourly_pt)
              }}
              pt/h</template
            >
            <template v-else-if="column.key === 'flags'"
              >{{ yesNo(allocationRecord(record).configured) }} /
              {{ yesNo(allocationRecord(record).observed) }}</template
            >
            <template v-else-if="column.key === 'usage'"
              ><span>{{ usageText(allocationRecord(record)) }}</span>
              <div
                v-for="note in metricNotes(allocationRecord(record))"
                :key="note.text"
                class="text-xs"
              >
                <Tag
                  :color="note.warning ? 'warning' : undefined"
                  class="m-0 whitespace-normal"
                  >{{ note.text }}</Tag
                >
              </div></template
            >
            <template v-else-if="column.key === 'release'"
              ><span v-if="releasePending(allocationRecord(record))"
                >释放待确认</span
              ><span
                v-else-if="
                  allocationRecord(record).status === 'released' &&
                  allocationRecord(record).release_verified
                "
                >已独立验证释放
                <TimeText :value="allocationRecord(record).released_at" /></span
              ><span v-else>—</span></template
            >
          </template>
        </Table>
      </div>
      <div class="gpu-mobile space-y-3">
        <Spin v-if="allocationLoading && !allocationLoaded" />
        <Empty
          v-else-if="allocationLoaded && !rows.length && !allocationError"
          description="暂无 GPU 分配"
        />
        <article
          v-for="allocation in rows"
          :key="allocation.id"
          class="min-w-0 rounded border p-3"
        >
          <label class="flex items-start gap-2">
            <input
              v-model="selected"
              type="checkbox"
              :value="allocation.id"
              :disabled="locked || allocation.status !== 'active'"
              :aria-label="`选择 GPU 分配 ${allocation.id}`"
              class="mt-1"
            />
            <strong class="min-w-0 break-words">{{ allocation.model }}</strong>
          </label>
          <p class="my-2 break-all font-mono text-xs">{{ allocation.uuid }}</p>
          <Tag>{{
            enumLabel('gpuAttachment', gpuAttachmentState(allocation)).text
          }}</Tag>
          <p class="mt-2 text-sm">
            分配单价快照：{{ formatPt(allocation.gpu_hourly_pt) }} Pt/卡/小时
          </p>
          <p class="text-sm">
            已配置 / 已观测：{{ yesNo(allocation.configured) }} /
            {{ yesNo(allocation.observed) }}
          </p>
          <p v-if="releasePending(allocation)" class="text-destructive text-sm">
            释放待确认
          </p>
          <p
            v-else-if="
              allocation.status === 'released' && allocation.release_verified
            "
            class="text-sm"
          >
            已独立验证释放 <TimeText :value="allocation.released_at" />
          </p>
          <p class="text-muted-foreground text-xs">
            本实例归因：{{ usageText(allocation) }}
          </p>
          <p
            v-for="note in metricNotes(allocation)"
            :key="note.text"
            class="text-xs"
          >
            <Tag
              :color="note.warning ? 'warning' : undefined"
              class="m-0 whitespace-normal"
              >{{ note.text }}</Tag
            >
          </p>
        </article>
      </div>
      <ErrorFeedback class="mt-2" :feedback="allocationError" /><ErrorFeedback
        class="mt-2"
        :feedback="metricError"
      />
    </Card>
    <Card size="small" title="GPU 策略">
      <p
        v-if="policy && !allowed('gpu_policy')"
        class="text-muted-foreground text-sm"
      >
        管理员未授权修改 GPU 策略；以下配置仅供查看。
      </p>
      <Spin :spinning="policyLoading && !policy">
        <template v-if="policy && draft">
          <Descriptions :column="{ xs: 1, sm: 2 }" size="small">
            <DescriptionsItem label="当前生效空闲回收"
              >{{ policy.effective_policy.idle.enabled ? '开启' : '关闭' }} ·
              {{ policy.effective_policy.idle.timeout_seconds }} 秒 ·
              {{
                policy.effective_policy.idle.action === 'stop_instance'
                  ? '停止实例'
                  : '卸载 GPU'
              }}</DescriptionsItem
            >
            <DescriptionsItem label="当前生效自动均衡"
              >{{ policy.effective_policy.rebalance.enabled ? '开启' : '关闭' }}
              ·
              {{
                policy.effective_policy.rebalance.allow_live
                  ? '允许运行中换卡'
                  : '不允许运行中换卡'
              }}
              ·
              {{ formatPt(policy.effective_policy.rebalance.max_hourly_pt) }}
              pt/卡/小时</DescriptionsItem
            >
            <DescriptionsItem label="空闲起始"
              ><TimeText :value="policy.idle_since" /></DescriptionsItem
            ><DescriptionsItem label="上次换卡"
              ><TimeText :value="policy.last_rebound_at"
            /></DescriptionsItem>
            <DescriptionsItem label="未生效原因"
              ><span v-if="!policy.blocked_reasons.length">无</span
              ><span v-else class="break-words">{{
                policy.blocked_reasons
                  .map((reason) => blockedNames[reason] ?? reason)
                  .join('；')
              }}</span></DescriptionsItem
            >
          </Descriptions>
          <Collapse ghost size="small" class="my-2"
            ><CollapsePanel key="safety" header="平台安全规则（只读）"
              ><Descriptions :column="{ xs: 1, sm: 2 }" size="small"
                ><DescriptionsItem
                  v-for="(value, key) in policy.effective_policy.safety"
                  :key="key"
                  :label="safetyNames[key] ?? key"
                  >{{
                    typeof value === 'boolean' ? (value ? '是' : '否') : value
                  }}</DescriptionsItem
                ></Descriptions
              ></CollapsePanel
            ></Collapse
          >
          <Alert
            v-if="draft.rebalance.enabled && !draft.rebalance.allow_live"
            class="mb-3"
            type="warning"
            show-icon
            message="自动均衡已开启，但未授权在线换卡：实例运行中不会执行；保存不会暗中开启 allow_live。"
          />
          <Form layout="vertical">
            <div class="grid min-w-0 grid-cols-1 gap-4 md:grid-cols-2">
              <section
                class="min-w-0 rounded border p-3"
                aria-label="空闲回收策略"
              >
                <h3 class="mb-3 text-sm font-medium">空闲回收</h3>
                <FormItem
                  label="开启空闲回收"
                  :extra="
                    readOnlyHint('idle') ||
                    (!policy.allowed_ranges.idle.enabled
                      ? '绑定政策未启用空闲回收，不能开启'
                      : '')
                  "
                  ><Switch
                    v-model:checked="draft.idle.enabled"
                    aria-label="开启空闲回收"
                    :disabled="
                      fieldDisabled('idle') ||
                      (!policy.allowed_ranges.idle.enabled &&
                        !draft.idle.enabled)
                    "
                /></FormItem>
                <FormItem
                  label="空闲时长（秒）"
                  :extra="
                    readOnlyHint('idle_timeout') ||
                    `${policy.allowed_ranges.idle.min_timeout_seconds}–${policy.allowed_ranges.idle.max_timeout_seconds} 秒`
                  "
                  ><InputNumber
                    v-model:value="draft.idle.timeout_seconds"
                    class="!w-full"
                    aria-label="空闲时长"
                    :disabled="fieldDisabled('idle_timeout')"
                    :precision="0"
                    :min="policy.allowed_ranges.idle.min_timeout_seconds"
                    :max="policy.allowed_ranges.idle.max_timeout_seconds"
                /></FormItem>
                <FormItem label="空闲动作" :extra="readOnlyHint('idle_action')"
                  ><Select
                    v-model:value="draft.idle.action"
                    aria-label="空闲动作"
                    :disabled="fieldDisabled('idle_action')"
                    :options="idleActions"
                /></FormItem>
              </section>
              <section
                class="min-w-0 rounded border p-3"
                aria-label="自动均衡策略"
              >
                <h3 class="mb-3 text-sm font-medium">自动均衡</h3>
                <FormItem
                  label="开启自动均衡"
                  :extra="readOnlyHint('rebalance')"
                  ><Switch
                    v-model:checked="draft.rebalance.enabled"
                    aria-label="开启自动均衡"
                    :disabled="fieldDisabled('rebalance')"
                /></FormItem>
                <FormItem
                  label="允许运行中换卡"
                  :extra="
                    readOnlyHint('allow_live') ||
                    'CUDA 上下文不会迁移；授权不绕过占用、归属及平台安全检查。'
                  "
                  ><Switch
                    v-model:checked="draft.rebalance.allow_live"
                    aria-label="允许运行中换卡"
                    :disabled="fieldDisabled('allow_live')"
                /></FormItem>
                <FormItem
                  label="单卡 max_hourly_pt（pt/h）"
                  :extra="
                    readOnlyHint('max_hourly_pt') ||
                    `${formatPt(policy.allowed_ranges.rebalance.min_hourly_pt)}–${formatPt(policy.allowed_ranges.rebalance.max_hourly_pt)} pt/h；0 不是不限价`
                  "
                  ><Input
                    v-model:value="draft.rebalance.max_hourly_pt"
                    aria-label="均衡单卡价格上限"
                    inputmode="decimal"
                    :disabled="fieldDisabled('max_hourly_pt')"
                /></FormItem>
              </section>
            </div>
            <p
              v-if="policyProblem"
              class="text-destructive mt-3 text-sm"
              role="status"
            >
              {{ policyProblem }}
            </p>
            <div class="mt-3 flex flex-wrap items-center gap-2">
              <Button
                type="primary"
                :disabled="!canPolicy || !dirty || Boolean(policyProblem)"
                @click="openDialog('policy')"
                >保存策略</Button
              ><Button :loading="policyLoading" @click="loadPolicy"
                >刷新（保留草稿）</Button
              ><Button
                v-if="requiresReview || conflict"
                :disabled="locked || policyLoading || Boolean(policyError)"
                @click="openDialog('review')"
                >核对最新版本并保留草稿</Button
              ><Button
                v-if="dirty"
                :disabled="locked"
                @click="openDialog('discard')"
                >放弃本地修改</Button
              ><span v-if="dirty" class="text-muted-foreground text-xs"
                >有未保存修改</span
              >
            </div>
          </Form>
        </template>
        <ErrorFeedback :feedback="policyError" />
      </Spin>
    </Card>
    <ConfirmAction
      v-model:open="dialogOpen"
      :title="dialogTitle"
      :disabled="Boolean(dialogProblem) || action.pending.value"
      :danger="dialogKind === 'detach' || dialogKind === 'discard'"
      :risk-text="dialogRisk"
      :description="
        dialogKind === 'policy'
          ? '提交完整 idle / rebalance 配置，使用最新核对的策略 ETag；不更改用户动作授权或字段权限。'
          : dialogKind === 'review'
            ? '仅更新草稿的版本基线，不会自动保存。请对照下方服务器当前配置；本地修改仍保留，未授权字段不得更改。'
            : dialogKind === 'discard'
              ? '明确放弃未保存的本地策略，使用最近成功读取的配置。'
              : '本次变更仍需通过服务器安全、容量、额度和占用检查；受理不等于完成，未知不代表释放成功。'
      "
      :on-confirm="confirmDialog"
    >
      <template
        v-if="
          dialogKind === 'attach' ||
          dialogKind === 'rebind' ||
          dialogKind === 'detach'
        "
      >
        <p v-if="dialogKind !== 'attach'">
          所选分配 {{ activeSelected.length }} 张：<span
            class="break-all font-mono"
            >{{
              activeSelected.map((item) => shortId(item.uuid)).join('、')
            }}</span
          >
        </p>
        <p v-if="binding && dialogKind !== 'detach'" class="break-words">
          绑定型号：{{ binding.model }} ·
          {{ binding.access_mode === 'shared' ? '共享' : '独占' }} ·
          {{ formatPt(binding.gpu_hourly_pt) }} pt/卡/小时
        </p>
        <Form layout="vertical"
          ><FormItem v-if="dialogKind === 'attach'" label="挂载数量"
            ><InputNumber
              v-model:value="count"
              aria-label="挂载数量"
              :disabled="locked"
              :min="1"
              :max="64"
              :precision="0"
              class="!w-full" /></FormItem
          ><FormItem
            v-if="dialogKind !== 'detach'"
            label="单卡 max_hourly_pt（pt/h）"
            ><Input
              v-model:value="maxHourly"
              aria-label="本次单卡价格上限"
              :disabled="locked"
              inputmode="decimal" /></FormItem
        ></Form>
      </template>
      <Descriptions
        v-if="dialogKind === 'review' && policy"
        :column="1"
        size="small"
        ><DescriptionsItem label="服务器当前空闲策略"
          >{{ policy.idle.enabled ? '开启' : '关闭' }} ·
          {{ policy.idle.timeout_seconds }} 秒 ·
          {{ policy.idle.action }}</DescriptionsItem
        ><DescriptionsItem label="服务器当前均衡策略"
          >{{ policy.rebalance.enabled ? '开启' : '关闭' }} · allow_live={{
            policy.rebalance.allow_live
          }}
          · {{ policy.rebalance.max_hourly_pt }} pt/h</DescriptionsItem
        ></Descriptions
      >
      <p
        v-if="dialogProblem"
        class="text-destructive break-words text-sm"
        role="alert"
      >
        {{ dialogProblem }}
      </p>
    </ConfirmAction>
  </div>
</template>

<style scoped>
.gpu-mobile {
  display: none;
}
@media (max-width: 767px) {
  .gpu-desktop {
    display: none;
  }
  .gpu-mobile {
    display: block;
  }
}
</style>
