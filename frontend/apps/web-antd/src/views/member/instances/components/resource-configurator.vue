<script lang="ts" setup>
import type { Plan, Resources } from '#/api/types';
import type { CreationResourceKey } from '#/utils/creation-math';

import { computed, ref, watch } from 'vue';

import { Alert, Input, Slider, Tag } from 'ant-design-vue';

import {
  bytesToExactGib,
  creationResourceBound,
  creationResourceError,
  exactGibToBytes,
  resourceFromSlider,
  resourceSliderIndex,
  resourceSliderMaximum,
} from '#/utils/creation-math';
import { formatBytes } from '#/utils/format';

const props = defineProps<{
  disabled?: boolean;
  modelValue: Resources;
  plan: Plan;
}>();
const emit = defineEmits<{
  'update:modelValue': [resources: Resources];
  validityChange: [valid: boolean];
}>();

const fields = [
  { key: 'vcpus', label: 'CPU', unit: 'vCPU' },
  { key: 'memory_bytes', label: '内存', unit: 'GiB' },
  { key: 'disk_bytes', label: '磁盘', unit: 'GiB' },
] as const;

function resourceDraft(resources: Resources): Record<CreationResourceKey, string> {
  return {
    disk_bytes: bytesToExactGib(resources.disk_bytes),
    memory_bytes: bytesToExactGib(resources.memory_bytes),
    vcpus: String(resources.vcpus),
  };
}

const draft = ref(resourceDraft(props.modelValue));

let changingInput = false;
const parsed = computed<Resources | null>(() => {
  const cpu = draft.value.vcpus.trim();
  const memory = exactGibToBytes(draft.value.memory_bytes);
  const disk = exactGibToBytes(draft.value.disk_bytes);
  if (!/^\d+$/.test(cpu) || !Number.isSafeInteger(Number(cpu)) || memory === null || disk === null) return null;
  return { disk_bytes: disk, memory_bytes: memory, vcpus: Number(cpu) };
});

const error = computed(() => {
  if (!/^\d+$/.test(draft.value.vcpus.trim()) || !Number.isSafeInteger(Number(draft.value.vcpus))) {
    return 'CPU 请输入有效的整数 vCPU 数量';
  }
  for (const key of ['memory_bytes', 'disk_bytes'] as const) {
    if (exactGibToBytes(draft.value[key]) === null) {
      return `${key === 'memory_bytes' ? '内存' : '磁盘'}请输入能精确换算为整数字节的 GiB 数值，不会自动舍入`;
    }
  }
  return parsed.value ? creationResourceError(props.plan, parsed.value) : '请输入有效的资源数值';
});

const rows = computed(() => fields.map((field) => {
  const bound = creationResourceBound(props.plan, field.key);
  const sliderMax = bound ? resourceSliderMaximum(bound) : null;
  const value = parsed.value ? BigInt(parsed.value[field.key]) : null;
  const sliderIndex = bound && value !== null ? resourceSliderIndex(bound, value) : null;
  let hint = '套餐资源范围无效，请重新选择套餐';
  if (bound) {
    if (bound.fixed) {
      hint = field.key === 'vcpus'
        ? `套餐固定配置：${bound.min} vCPU`
        : `套餐固定配置：${formatBytes(bound.min.toString())}（${bound.min} 字节）`;
    } else {
      const min = field.key === 'vcpus' ? bound.min.toString() : bytesToExactGib(bound.min.toString());
      const max = field.key === 'vcpus' ? bound.max.toString() : bytesToExactGib(bound.max.toString());
      const step = field.key === 'vcpus' ? bound.step.toString() : bytesToExactGib(bound.step.toString());
      hint = `范围 ${min}–${max} ${field.unit}，从下限起每次增加 ${step} ${field.unit}`;
    }
  }
  return { ...field, bound, hint, sliderIndex, sliderMax };
}));

// 父级回填只同步真正变化的资源，保留用户合法输入中的小数写法和光标位置。
watch(() => props.modelValue, (resources) => {
  const current = parsed.value;
  if (!current || current.vcpus !== resources.vcpus
    || current.memory_bytes !== resources.memory_bytes || current.disk_bytes !== resources.disk_bytes) {
    draft.value = resourceDraft(resources);
  }
}, { deep: true, flush: 'sync' });
watch(() => props.plan, () => {
  draft.value = resourceDraft(props.modelValue);
}, { deep: true, flush: 'sync' });
watch(error, (problem) => {
  if (problem !== null || !changingInput) emit('validityChange', problem === null);
}, { flush: 'sync', immediate: true });

function changeInput(key: CreationResourceKey, value: string) {
  if (props.disabled || creationResourceBound(props.plan, key)?.fixed) return;
  changingInput = true;
  draft.value[key] = value;
  if (error.value === null && parsed.value) emit('update:modelValue', { ...parsed.value });
  changingInput = false;
  if (error.value === null) emit('validityChange', true);
}

function changeSlider(key: CreationResourceKey, index: number | number[]) {
  if (props.disabled || Array.isArray(index)) return;
  const bound = creationResourceBound(props.plan, key);
  if (!bound || bound.fixed || resourceSliderMaximum(bound) === null) return;
  const value = resourceFromSlider(bound, index);
  if (value === null) return;
  changeInput(key, key === 'vcpus' ? value.toString() : bytesToExactGib(value.toString()));
}
</script>

<template>
  <div class="space-y-6">
    <div v-for="row in rows" :key="row.key" class="space-y-2">
      <div class="flex flex-wrap items-center justify-between gap-3">
        <label :for="`creation-resource-${row.key}`" class="font-medium">{{ row.label }}</label>
        <Tag v-if="row.bound?.fixed" class="!m-0">套餐固定</Tag>
      </div>
      <div class="flex flex-col gap-3 sm:flex-row sm:items-center sm:gap-5">
        <Slider
          v-if="row.bound && !row.bound.fixed && row.sliderMax !== null && row.sliderMax > 0"
          class="!mx-2 min-w-0 flex-1"
          :aria-label-for-handle="`${row.label}步数`"
          :disabled="disabled || row.sliderIndex === null"
          :min="0"
          :max="row.sliderMax"
          :step="1"
          :tooltip-open="false"
          :value="row.sliderIndex ?? 0"
          @change="changeSlider(row.key, $event)"
        />
        <div v-else-if="row.bound && !row.bound.fixed" class="flex-1 text-sm text-muted-foreground">
          {{ row.sliderMax === null ? '范围较大，请使用右侧精确输入' : '当前范围仅有一个合法值' }}
        </div>
        <Input
          :id="`creation-resource-${row.key}`"
          class="!w-full sm:!w-60"
          :aria-describedby="`creation-resource-hint-${row.key}`"
          :aria-invalid="!!error"
          :disabled="disabled || !row.bound || row.bound.fixed"
          :inputmode="row.key === 'vcpus' ? 'numeric' : 'decimal'"
          :status="error ? 'error' : undefined"
          :suffix="row.unit"
          :value="draft[row.key]"
          @update:value="changeInput(row.key, $event)"
        />
      </div>
      <p :id="`creation-resource-hint-${row.key}`" class="break-all text-xs leading-5 text-muted-foreground">
        {{ row.hint }}
      </p>
    </div>
    <Alert v-if="error" type="error" show-icon :message="error" role="alert" />
  </div>
</template>
