<script lang="ts" setup>
/**
 * 修改实例规格（运行中须确认重启）。custom：自由资源；elastic：受创建时冻结的套餐资源与 allowed_overrides 限制。
 * 磁盘不可缩容。后端没有调整规格的报价接口，不显示新费用。
 */
import type { Instance, OverrideKey, ResizeLimits, Resources } from '#/api/types';

import { computed, onBeforeUnmount, ref, watch } from 'vue';

import { Alert, Button, Form, FormItem, InputNumber } from 'ant-design-vue';

import type { instancesApi } from '#/api/backend';
import { ApiError, newIdempotencyKey } from '#/api/request';
import ConfirmAction from '#/components/confirm-action.vue';
import ErrorFeedback from '#/components/error-feedback.vue';
import { errorToast, trackAccepted } from '#/composables/api';
import { describeError } from '#/locales/errors';
import { bytesToGib, formatBytes, gibToBytes } from '#/utils/format';

const props = defineProps<{ api: Pick<typeof instancesApi, 'resize' | 'resizeLimits'>; etag: null | string; instance: Instance; disabled?: boolean }>();
const emit = defineEmits<{ done: []; unresolved: [value: boolean]; submitting: [value: boolean] }>();
const open = defineModel<boolean>('open', { default: false });

const GIB = 1024n ** 3n;
const form = ref({ disk_gib: 0, memory_gib: 0, vcpus: 0 });
const pending = ref(false);
watch(pending, (value) => emit('submitting', value), { flush: 'sync' });
const frozen = ref<null | { id: string; body: { confirm_restart?: boolean; resources: Resources }; etag: string | null; key: string }>(null);
watch(frozen, (value) => emit('unresolved', !!value), { flush: 'sync' });
let generation = 0;
const elastic = computed(() => props.instance.product_type === 'elastic');
const snapshot = computed(() => props.instance.plan_snapshot);
const running = computed(() => props.instance.observed_state === 'running');
const lifecycleDisabled = computed(() => props.disabled || props.instance.provisioning_state !== 'ready'
  || props.instance.state_stale || !!props.instance.active_task_id || !props.instance.capabilities.resize
  || !['running', 'stopped'].includes(props.instance.observed_state));

const limits = ref<null | ResizeLimits>(null);
const limitsLoading = ref(false);
const limitsError = ref<unknown>(null);
let limitsRequest = 0;

async function loadLimits() {
  const request = ++limitsRequest;
  const id = props.instance.id;
  limits.value = null;
  limitsError.value = null;
  limitsLoading.value = true;
  try {
    const result = await props.api.resizeLimits(id);
    if (request === limitsRequest && open.value && props.instance.id === id) limits.value = result.data;
  } catch (error) {
    if (request === limitsRequest && open.value && props.instance.id === id) limitsError.value = error;
  } finally {
    if (request === limitsRequest) limitsLoading.value = false;
  }
}

watch([open, () => props.instance.id], ([visible]) => {
  if (frozen.value && frozen.value.id === props.instance.id) return;
  ++generation;
  ++limitsRequest;
  limits.value = null;
  limitsError.value = null;
  limitsLoading.value = false;
  if (!visible) return;
  const r = props.instance.resources;
  form.value = { disk_gib: gibLimit(BigInt(r.disk_bytes), 'ceil'), memory_gib: bytesToGib(r.memory_bytes), vcpus: r.vcpus };
  frozen.value = null;
  void loadLimits();
}, { flush: 'sync', immediate: true });
onBeforeUnmount(() => { ++limitsRequest; ++generation; });

type ResKey = Exclude<OverrideKey, 'gpu_count'>;
interface Bound {
  fixed: boolean;
  max: bigint;
  min: bigint;
  step: bigint;
}

/** 每项上限是该实例可调整到的总量，不是可增加量。 */
function bound(key: ResKey): Bound {
  const current = BigInt(props.instance.resources[key]);
  const floor = key === 'disk_bytes' ? current : key === 'memory_bytes' ? GIB / 1000n : 1n;
  let max = limits.value ? BigInt(limits.value.max_resources[key]) : 0n;
  if (key === 'vcpus' && max > 65_535n) max = 65_535n;
  if (!elastic.value) return { fixed: false, max, min: floor, step: 1n };
  const p = snapshot.value;
  const o = p?.allowed_overrides[key];
  if (!p || !o) {
    const v = BigInt(p ? p[key] : props.instance.resources[key]);
    return { fixed: true, max: max < v ? max : v, min: v > floor ? v : floor, step: 1n };
  }
  const base = BigInt(o.min);
  const step = BigInt(o.step);
  let min = base > floor ? base : floor;
  if (step > 0n) min = base + ((min - base + step - 1n) / step) * step;
  const planMax = BigInt(o.max);
  if (planMax < max) max = planMax;
  // 截断到从套餐原始 min 起算的最后一个合法步进，不能从新下限重新起算。
  if (step > 0n && max >= base) max = base + ((max - base) / step) * step;
  return { fixed: false, max, min, step };
}

/** 先用 BigInt 向内取整，再转换为输入框的三位小数 GiB。 */
function gibLimit(bytes: bigint, rounding: 'ceil' | 'floor'): number {
  const scaled = bytes * 1000n;
  const units = rounding === 'ceil' ? (scaled + GIB - 1n) / GIB : scaled / GIB;
  return Number(units) / 1000;
}

function inputLimit(key: ResKey, side: 'max' | 'min'): number {
  const value = bound(key)[side];
  return key === 'vcpus' ? Number(value) : gibLimit(value, side === 'min' ? 'ceil' : 'floor');
}

function unavailable(key: ResKey): boolean {
  const b = bound(key);
  return b.max < b.min || inputLimit(key, 'max') < inputLimit(key, 'min');
}

const inputsDisabled = computed(() => !!frozen.value || pending.value || !limits.value || limitsLoading.value || (elastic.value && !snapshot.value));

function toBytes(gib: number): null | string {
  try {
    return gibToBytes(gib);
  } catch {
    return null;
  }
}

const resources = computed<null | Resources>(() => {
  const m = toBytes(form.value.memory_gib);
  const d = toBytes(form.value.disk_gib);
  if (!m || !d || !Number.isInteger(form.value.vcpus)) return null;
  return { disk_bytes: d, memory_bytes: m, vcpus: form.value.vcpus };
});

const NAMES: Record<ResKey, string> = { disk_bytes: '磁盘', memory_bytes: '内存', vcpus: 'vCPU' };
const show = (key: ResKey, v: bigint) => (key === 'vcpus' ? String(v) : formatBytes(String(v)));

const problems = computed(() => {
  if (!limits.value || limitsLoading.value) return [limitsLoading.value ? '正在读取配额和节点容量，暂不可提交' : '可调整范围不可用，请重试后再提交'];
  const r = resources.value;
  if (!r) return ['请输入有效的资源数值（容量最多 3 位小数）'];
  if (elastic.value && !snapshot.value) return ['创建时的套餐配置不可用，无法按冻结范围调整'];
  const out: string[] = [];
  for (const key of ['vcpus', 'memory_bytes', 'disk_bytes'] as const) {
    const v = BigInt(r[key]);
    const b = bound(key);
    const input = key === 'vcpus' ? form.value.vcpus : key === 'memory_bytes' ? form.value.memory_gib : form.value.disk_gib;
    if (unavailable(key)) out.push(`${NAMES[key]}在当前配额和节点容量下无法调整：允许上限 ${inputLimit(key, 'max')}${key === 'vcpus' ? '' : ' GiB'} 低于下限 ${inputLimit(key, 'min')}${key === 'vcpus' ? '' : ' GiB'}`);
    else if (b.fixed && v !== b.min) out.push(`${NAMES[key]}不可调整，必须为 ${show(key, b.min)}`);
    else if (v < b.min || input < inputLimit(key, 'min')) out.push(`${NAMES[key]}不能小于 ${inputLimit(key, 'min')}${key === 'vcpus' ? '' : ' GiB'}${key === 'disk_bytes' ? '（禁止缩容）' : ''}`);
    else if (v > b.max || input > inputLimit(key, 'max')) out.push(`${NAMES[key]}不能大于 ${inputLimit(key, 'max')}${key === 'vcpus' ? '' : ' GiB'}`);
    else if (elastic.value && b.step > 0n) {
      const o = snapshot.value?.allowed_overrides[key];
      if (o && (v - BigInt(o.min)) % b.step !== 0n) out.push(`${NAMES[key]}必须按 ${show(key, b.step)} 递增`);
    }
  }
  return out;
});

function hint(key: ResKey): string {
  if (!limits.value) return limitsLoading.value ? '正在读取允许总量…' : '允许总量不可用';
  const b = bound(key);
  if (unavailable(key)) return '当前配额和节点容量下无法调整（上限低于下限）';
  const unit = key === 'vcpus' ? '' : ' GiB';
  if (b.fixed) return `固定总量 ${inputLimit(key, 'min')}${unit}`;
  return `允许总量 ${inputLimit(key, 'min')} – ${inputLimit(key, 'max')}${unit}${elastic.value ? `，步长 ${show(key, b.step)}` : ''}`;
}
const stepGib = (key: 'disk_bytes' | 'memory_bytes') => {
  const b = bound(key);
  return elastic.value && !b.fixed ? Math.max(0.001, Number(b.step) / Number(GIB)) : 1;
};

async function submit() {
  if (pending.value || props.disabled) return false;
  if (!frozen.value) {
    const r = resources.value;
    if (!r || lifecycleDisabled.value || problems.value.length > 0) return false;
    const body: { confirm_restart?: boolean; resources: Resources } = { resources: { ...r } };
    if (running.value) body.confirm_restart = true;
    frozen.value = { id: props.instance.id, body, etag: props.etag, key: newIdempotencyKey() };
  }
  const request = frozen.value;
  const token = generation;
  pending.value = true;
  try {
    const res = await props.api.resize(request.id, request.body, { ifMatch: request.etag, key: request.key });
    if (token !== generation) return false;
    frozen.value = null;
    trackAccepted(res.data, '修改实例规格');
    emit('done');
    return true;
  } catch (failure) {
    if (token !== generation) return false;
    if (failure instanceof ApiError && failure.status > 0 && failure.status !== 408 && failure.status < 500) {
      frozen.value = null;
      emit('done');
    }
    errorToast(failure);
    return false;
  } finally { if (token === generation) pending.value = false; }
}
</script>

<template>
  <ConfirmAction
    v-model:open="open"
    :description="`${running ? '执行此操作将重启实例：正常停止后修改规格，再重新启动。' : ''}${elastic ? '标准实例受创建时冻结的套餐范围限制，后续套餐编辑不会改变此范围。' : '自定义实例不受套餐范围限制。'}允许总量按用户配额、节点容量及常驻预算计算，已排除当前实例占用；提交时重新校验。`"
    :disabled="pending || (!frozen && (lifecycleDisabled || inputsDisabled || problems.length > 0))"
    :on-confirm="submit"
    :risk-text="running ? '我确认执行此操作将重启实例，期间服务会中断' : ''"
    title="修改实例规格"
  >
    <Alert v-if="frozen" class="mb-3" type="warning" message="提交结果尚未确认，原配置和请求已锁定。再次确认仅重试原请求，请勿重复发起规格修改。" show-icon />
    <Alert v-if="limitsLoading" class="mb-3" message="正在读取所有者配额和节点可用容量，暂不可提交。" show-icon type="info" />
    <Alert v-else-if="limitsError" class="mb-3" :message="describeError(limitsError).text" show-icon type="error">
      <template #description><ErrorFeedback :feedback="describeError(limitsError)" hide-message /></template>
    </Alert>
    <div class="mb-3 flex justify-end">
      <Button :loading="limitsLoading" size="small" @click="loadLimits">{{ limitsError ? '重试读取范围' : '刷新可调整范围' }}</Button>
    </div>
    <Form layout="vertical">
      <div class="grid grid-cols-3 gap-x-3">
        <FormItem :help="hint('vcpus')" label="vCPU">
          <InputNumber
            v-model:value="form.vcpus"
            class="w-full"
            :disabled="inputsDisabled || bound('vcpus').fixed || unavailable('vcpus')"
            :max="inputLimit('vcpus', 'max')"
            :min="inputLimit('vcpus', 'min')"
            :precision="0"
          />
        </FormItem>
        <FormItem :help="hint('memory_bytes')" label="内存（GiB）">
          <InputNumber
            v-model:value="form.memory_gib"
            class="w-full"
            :disabled="inputsDisabled || bound('memory_bytes').fixed || unavailable('memory_bytes')"
            :max="inputLimit('memory_bytes', 'max')"
            :min="inputLimit('memory_bytes', 'min')"
            :precision="3"
            :step="stepGib('memory_bytes')"
          />
        </FormItem>
        <FormItem :help="hint('disk_bytes')" label="磁盘（GiB）">
          <InputNumber
            v-model:value="form.disk_gib"
            class="w-full"
            :disabled="inputsDisabled || bound('disk_bytes').fixed || unavailable('disk_bytes')"
            :max="inputLimit('disk_bytes', 'max')"
            :min="inputLimit('disk_bytes', 'min')"
            :precision="3"
            :step="stepGib('disk_bytes')"
          />
        </FormItem>
      </div>
    </Form>
    <Alert v-if="problems.length > 0" class="mb-3" :message="problems.join('；')" show-icon type="warning" />
  </ConfirmAction>
</template>
