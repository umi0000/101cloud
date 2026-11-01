<script lang="ts" setup>
/**
 * 调整规格（实例须已停止）。custom：自由资源；elastic：受套餐 allowed_overrides 限制并发送套餐 policy_versions。
 * 磁盘不可缩容。后端没有调整规格的报价接口，不显示新费用。
 */
import type { Instance, OverrideKey, Plan, ResizeLimits, Resources } from '#/api/types';

import { computed, onBeforeUnmount, ref, watch } from 'vue';

import { Alert, Button, Form, FormItem, InputNumber } from 'ant-design-vue';

import { instancesApi } from '#/api/backend';
import ConfirmAction from '#/components/confirm-action.vue';
import ErrorFeedback from '#/components/error-feedback.vue';
import { trackAccepted, useAction } from '#/composables/api';
import { describeError } from '#/locales/errors';
import { bytesToGib, formatBytes, gibToBytes } from '#/utils/format';

const props = defineProps<{ etag: null | string; instance: Instance; plan: null | Plan }>();
const emit = defineEmits<{ done: [] }>();
const open = defineModel<boolean>('open', { default: false });

const GIB = 1024n ** 3n;
const form = ref({ disk_gib: 0, memory_gib: 0, vcpus: 0 });
const action = useAction();
const elastic = computed(() => props.instance.product_type === 'elastic');

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
    const result = await instancesApi.resizeLimits(id);
    if (request === limitsRequest && open.value && props.instance.id === id) limits.value = result.data;
  } catch (error) {
    if (request === limitsRequest && open.value && props.instance.id === id) limitsError.value = error;
  } finally {
    if (request === limitsRequest) limitsLoading.value = false;
  }
}

watch([open, () => props.instance.id], ([visible]) => {
  ++limitsRequest;
  limits.value = null;
  limitsError.value = null;
  limitsLoading.value = false;
  if (!visible) return;
  const r = props.instance.resources;
  form.value = { disk_gib: gibLimit(BigInt(r.disk_bytes), 'ceil'), memory_gib: bytesToGib(r.memory_bytes), vcpus: r.vcpus };
  action.resetKey();
  void loadLimits();
}, { flush: 'sync', immediate: true });
onBeforeUnmount(() => { ++limitsRequest; });
watch(form, () => action.resetKey(), { deep: true });

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
  const p = props.plan;
  const o = p?.allowed_overrides[key];
  if (!p || !o) {
    const v = BigInt(p ? p.resources[key] : props.instance.resources[key]);
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

const inputsDisabled = computed(() => !limits.value || limitsLoading.value || (elastic.value && !props.plan));

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
  if (elastic.value && !props.plan) return ['套餐信息不可用，无法按套餐范围调整'];
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
      const o = props.plan?.allowed_overrides[key];
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
  const r = resources.value;
  if (!r || problems.value.length > 0) return false;
  const body: Parameters<typeof instancesApi.resize>[1] = { resources: r };
  if (elastic.value && props.plan) body.policy_versions = { ...props.plan.policy_versions };
  const res = await action.run((key) => instancesApi.resize(props.instance.id, body, { ifMatch: props.etag, key }), {
    onConflict: () => emit('done'),
    success: '已提交调整规格',
  });
  if (!res) return false;
  trackAccepted(res.data, '调整规格', () => emit('done'));
  emit('done');
  return true;
}
</script>

<template>
  <ConfirmAction
    v-model:open="open"
    :description="`${elastic ? '标准实例还受套餐范围限制。' : '自定义实例不受套餐范围限制。'}允许总量按用户配额、节点容量及常驻预算计算，已排除当前实例占用；提交时重新校验。`"
    :disabled="inputsDisabled || problems.length > 0"
    :on-confirm="submit"
    title="调整规格"
  >
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
