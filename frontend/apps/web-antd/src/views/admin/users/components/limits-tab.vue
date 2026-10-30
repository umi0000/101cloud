<script lang="ts" setup>
import type { Limits, LimitsInput } from '#/api/types';

import { computed, onMounted, ref, watch } from 'vue';

import { Alert, Button, Form, FormItem, InputNumber, Skeleton } from 'ant-design-vue';

import { usersApi } from '#/api/backend';
import { useAction, useResource } from '#/composables/api';
import { bytesToGib, formatBytes, gibToBytes } from '#/utils/format';

const props = defineProps<{ userId: string }>();

/** 后端计数字段为 uint32。 */
const UINT32_MAX = 4_294_967_295;
type CountField = 'max_gpus' | 'max_instances' | 'max_resident_instances' | 'max_vcpus';
type GibField = 'max_disk_gib' | 'max_memory_gib';
/** InputNumber 清空时运行时为 null，校验统一用 typeof。 */
type FormState = Record<CountField | GibField, number | undefined>;

const limits = useResource(() => usersApi.limits(props.userId));
const form = ref<FormState>(empty());

function empty(): FormState {
  return {
    max_disk_gib: undefined,
    max_gpus: undefined,
    max_instances: undefined,
    max_memory_gib: undefined,
    max_resident_instances: undefined,
    max_vcpus: undefined,
  };
}

function fill(l: Limits) {
  form.value = {
    max_disk_gib: bytesToGib(l.max_disk_bytes),
    max_gpus: l.max_gpus,
    max_instances: l.max_instances,
    max_memory_gib: bytesToGib(l.max_memory_bytes),
    max_resident_instances: l.max_resident_instances,
    max_vcpus: l.max_vcpus,
  };
}
watch(limits.data, (l) => l && fill(l));

const countFields: { field: CountField; help: string; label: string }[] = [
  { field: 'max_instances', help: '实例总数（含标准与自定义）', label: '最大实例数' },
  { field: 'max_resident_instances', help: '自定义（常驻）实例数，不能大于最大实例数', label: '最大自定义实例数' },
  { field: 'max_vcpus', help: '所有实例 vCPU 合计', label: '最大 vCPU' },
  { field: 'max_gpus', help: '同时持有的 GPU 分配数', label: '最大 GPU' },
];
const gibFields: { field: GibField; label: string; source: 'max_disk_bytes' | 'max_memory_bytes' }[] = [
  { field: 'max_memory_gib', label: '最大内存（GiB）', source: 'max_memory_bytes' },
  { field: 'max_disk_gib', label: '最大磁盘（GiB）', source: 'max_disk_bytes' },
];

/** GiB 未改动时原样保留字节值，避免非整 GiB 的旧值因换算被改写。 */
function bytesFor(field: GibField, source: 'max_disk_bytes' | 'max_memory_bytes'): null | string {
  const gib = form.value[field];
  const current = limits.data.value?.[source];
  if (typeof gib !== 'number') return null;
  if (current !== undefined && bytesToGib(current) === gib) return current;
  try {
    return gibToBytes(gib);
  } catch {
    return null;
  }
}

const body = computed<{ error: string } | { value: LimitsInput }>(() => {
  const f = form.value;
  const counts: Record<CountField, number> = { max_gpus: 0, max_instances: 0, max_resident_instances: 0, max_vcpus: 0 };
  for (const { field, label } of countFields) {
    const v = f[field];
    if (typeof v !== 'number' || !Number.isInteger(v) || v < 0 || v > UINT32_MAX) return { error: `${label}必须是非负整数` };
    counts[field] = v;
  }
  const memory = bytesFor('max_memory_gib', 'max_memory_bytes');
  const disk = bytesFor('max_disk_gib', 'max_disk_bytes');
  if (memory === null || disk === null) return { error: '容量必须是非负数字，最多 3 位小数' };
  if (counts.max_resident_instances > counts.max_instances) return { error: '最大自定义实例数不能大于最大实例数' };
  return {
    value: {
      max_disk_bytes: disk,
      max_gpus: counts.max_gpus,
      max_instances: counts.max_instances,
      max_memory_bytes: memory,
      max_resident_instances: counts.max_resident_instances,
      max_vcpus: counts.max_vcpus,
    },
  };
});

const save = useAction();
watch(form, () => save.resetKey(), { deep: true });

async function submit() {
  const b = body.value;
  if ('error' in b) return;
  const r = await save.run(() => usersApi.putLimits(props.userId, b.value, { ifMatch: limits.etag.value }), {
    onConflict: () => limits.reload(),
    success: '配额已保存',
  });
  if (r) await limits.reload();
}

onMounted(() => limits.reload().catch(() => undefined));
</script>

<template>
  <div class="max-w-2xl">
    <Alert
      class="mb-4"
      message="0 表示禁止新增该项资源。降低配额不会停止或删除已有实例，只影响之后的创建与调整。"
      show-icon
      type="info"
    />
    <Alert
      v-if="limits.error.value && !limits.data.value"
      message="配额加载失败"
      show-icon
      type="error"
    >
      <template #action><Button size="small" @click="limits.reload()">重试</Button></template>
    </Alert>
    <Skeleton v-else-if="!limits.data.value" active />
    <Form v-else layout="vertical">
      <div class="grid grid-cols-2 gap-x-4">
        <FormItem v-for="c in countFields" :key="c.field" :help="c.help" :label="c.label">
          <InputNumber v-model:value="form[c.field]" class="w-full" :max="UINT32_MAX" :min="0" :step="1" />
        </FormItem>
        <FormItem
          v-for="g in gibFields"
          :key="g.field"
          :help="`当前 ${formatBytes(limits.data.value?.[g.source])}`"
          :label="g.label"
        >
          <InputNumber v-model:value="form[g.field]" class="w-full" :min="0" />
        </FormItem>
      </div>
      <Alert v-if="'error' in body" class="mb-3" :message="body.error" show-icon type="warning" />
      <Button :disabled="'error' in body" :loading="save.pending.value" type="primary" @click="submit">
        保存配额
      </Button>
      <Button class="ml-2" @click="limits.data.value && fill(limits.data.value)">还原</Button>
    </Form>
  </div>
</template>
