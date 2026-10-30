<script lang="ts" setup>
/**
 * 调整规格（实例须已停止）。custom：自由资源；elastic：受套餐 allowed_overrides 限制并发送套餐 policy_versions。
 * 磁盘不可缩容。后端没有调整规格的报价接口，不显示新费用。
 */
import type { Instance, OverrideKey, Plan, Resources } from '#/api/types';

import { computed, ref, watch } from 'vue';

import { Alert, Form, FormItem, InputNumber } from 'ant-design-vue';

import { instancesApi } from '#/api/backend';
import ConfirmAction from '#/components/confirm-action.vue';
import { trackAccepted, useAction } from '#/composables/api';
import { bytesToGib, formatBytes, gibToBytes } from '#/utils/format';

const props = defineProps<{ etag: null | string; instance: Instance; plan: null | Plan }>();
const emit = defineEmits<{ done: [] }>();
const open = defineModel<boolean>('open', { default: false });

const GIB = 1024 ** 3;
const form = ref({ disk_gib: 0, memory_gib: 0, vcpus: 0 });
const action = useAction();
const elastic = computed(() => props.instance.product_type === 'elastic');

watch(open, (v) => {
  if (!v) return;
  const r = props.instance.resources;
  form.value = { disk_gib: bytesToGib(r.disk_bytes), memory_gib: bytesToGib(r.memory_bytes), vcpus: r.vcpus };
  action.resetKey();
});
watch(form, () => action.resetKey(), { deep: true });

type ResKey = Exclude<OverrideKey, 'gpu_count'>;
interface Bound {
  fixed: boolean;
  max: bigint | null;
  min: bigint;
  step: bigint;
}

/** 每项资源的允许范围（整数：vCPU 个数或字节数）。 */
function bound(key: ResKey): Bound {
  const current = BigInt(props.instance.resources[key]);
  const floor = key === 'disk_bytes' ? current : 1n;
  if (!elastic.value) return { fixed: false, max: key === 'vcpus' ? 65_535n : null, min: floor, step: 1n };
  const p = props.plan;
  const o = p?.allowed_overrides[key];
  if (!p || !o) {
    const v = BigInt(p ? p.resources[key] : props.instance.resources[key]);
    return { fixed: true, max: v, min: v, step: 1n };
  }
  const min = BigInt(o.min);
  return { fixed: false, max: BigInt(o.max), min: min > floor ? min : floor, step: BigInt(o.step) };
}

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
  const r = resources.value;
  if (!r) return ['请输入有效的资源数值（容量最多 3 位小数）'];
  if (elastic.value && !props.plan) return ['套餐信息不可用，无法按套餐范围调整'];
  const out: string[] = [];
  for (const key of ['vcpus', 'memory_bytes', 'disk_bytes'] as const) {
    const v = BigInt(r[key]);
    const b = bound(key);
    if (b.fixed && v !== b.min) out.push(`${NAMES[key]}不可调整，必须为 ${show(key, b.min)}`);
    else if (v < b.min) out.push(`${NAMES[key]}不能小于 ${show(key, b.min)}${key === 'disk_bytes' ? '（禁止缩容）' : ''}`);
    else if (b.max !== null && v > b.max) out.push(`${NAMES[key]}不能大于 ${show(key, b.max)}`);
    else if (elastic.value && b.step > 0n) {
      const o = props.plan?.allowed_overrides[key];
      if (o && (v - BigInt(o.min)) % b.step !== 0n) out.push(`${NAMES[key]}必须按 ${show(key, b.step)} 递增`);
    }
  }
  return out;
});

function hint(key: ResKey): string {
  const b = bound(key);
  if (b.fixed) return `固定为 ${show(key, b.min)}`;
  const max = b.max === null ? '不限' : show(key, b.max);
  return `${show(key, b.min)} – ${max}${elastic.value ? `，步长 ${show(key, b.step)}` : ''}`;
}
const stepGib = (key: 'disk_bytes' | 'memory_bytes') => {
  const b = bound(key);
  return elastic.value && !b.fixed ? Number(b.step) / GIB : 1;
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
    :description="elastic ? '标准实例按套餐允许范围调整，并使用套餐当前政策版本。' : '自定义实例不受套餐范围限制，仍检查配额和节点容量。'"
    :on-confirm="submit"
    title="调整规格"
  >
    <Form layout="vertical">
      <div class="grid grid-cols-3 gap-x-3">
        <FormItem :help="hint('vcpus')" label="vCPU">
          <InputNumber v-model:value="form.vcpus" class="w-full" :disabled="bound('vcpus').fixed" :min="1" :precision="0" />
        </FormItem>
        <FormItem :help="hint('memory_bytes')" label="内存（GiB）">
          <InputNumber
            v-model:value="form.memory_gib"
            class="w-full"
            :disabled="bound('memory_bytes').fixed"
            :min="0.001"
            :precision="3"
            :step="stepGib('memory_bytes')"
          />
        </FormItem>
        <FormItem :help="hint('disk_bytes')" label="磁盘（GiB）">
          <InputNumber
            v-model:value="form.disk_gib"
            class="w-full"
            :disabled="bound('disk_bytes').fixed"
            :min="0.001"
            :precision="3"
            :step="stepGib('disk_bytes')"
          />
        </FormItem>
      </div>
    </Form>
    <Alert v-if="problems.length > 0" class="mb-3" :message="problems.join('；')" show-icon type="warning" />
    <div class="text-muted-foreground mb-3 text-xs">后端没有调整规格的报价接口，这里不显示新费用。</div>
  </ConfirmAction>
</template>
