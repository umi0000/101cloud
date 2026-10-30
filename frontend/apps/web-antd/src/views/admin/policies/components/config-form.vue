<script lang="ts" setup>
import type { FormState } from './fields';

import type { PolicyKind } from '#/api/types';

import { computed } from 'vue';

import { AutoComplete, CheckboxGroup, FormItem, Input, InputNumber, Radio, RadioGroup, Switch } from 'ant-design-vue';

import { POLICY_FIELDS } from './fields';

const props = defineProps<{ errors: Record<string, string>; gpuModels?: string[]; kind: PolicyKind }>();
const state = defineModel<FormState>('state', { required: true });

const fields = computed(() => POLICY_FIELDS[props.kind]);
const modelOptions = computed(() => (props.gpuModels ?? []).map((value) => ({ value })));
</script>

<template>
  <div>
    <FormItem
      v-for="f in fields"
      :key="f.key"
      :help="errors[f.key] ?? f.hint"
      :validate-status="errors[f.key] ? 'error' : ''"
      :required="f.type !== 'bool' && !f.optional"
    >
      <template #label>
        <span>{{ f.label }}</span>
        <span class="text-muted-foreground ml-1 font-mono text-xs">{{ f.key }}</span>
      </template>
      <InputNumber
        v-if="f.type === 'int'"
        :value="state.numbers[f.key] ?? undefined"
        @update:value="(v: null | number | string) => (state.numbers[f.key] = typeof v === 'number' ? v : null)"
        class="w-60"
        :max="f.max"
        :min="f.min"
        :precision="0"
        :addon-after="f.unit"
      />
      <Input
        v-else-if="f.type === 'pt' || f.type === 'decimal'"
        v-model:value="state.texts[f.key]"
        class="w-60 font-mono"
        :addon-after="f.unit"
      />
      <AutoComplete
        v-else-if="f.key === 'gpu_model'"
        v-model:value="state.texts[f.key]"
        class="w-80"
        :filter-option="(input: string, o: { value: string }) => o.value.toLowerCase().includes(input.toLowerCase())"
        :options="modelOptions"
        placeholder="如 NVIDIA A100"
      />
      <Input
        v-else-if="f.type === 'text'"
        v-model:value="state.texts[f.key]"
        class="w-60 font-mono"
        :placeholder="f.placeholder"
      />
      <Switch v-else-if="f.type === 'bool'" v-model:checked="state.bools[f.key]" />
      <CheckboxGroup
        v-else-if="f.type === 'actions'"
        v-model:value="state.lists[f.key]"
        :options="(f.options ?? []).map((o) => ({ label: `${o.label}（${o.value}）`, value: o.value }))"
      />
      <RadioGroup v-else-if="f.type === 'choice'" v-model:value="state.texts[f.key]">
        <div class="flex flex-col gap-1">
          <Radio v-for="o in f.options" :key="o.value" :value="o.value">
            <span>{{ o.label }}</span>
            <span class="text-muted-foreground ml-1 font-mono text-xs">{{ o.value }}</span>
            <span v-if="o.hint" class="text-muted-foreground ml-2 text-xs">{{ o.hint }}</span>
          </Radio>
        </div>
      </RadioGroup>
    </FormItem>
  </div>
</template>
