<script lang="ts" setup>
/**
 * 通用确认弹窗：可要求输入名称确认、勾选风险项。
 * 弹窗只在无参 onConfirm 成功返回 true 后关闭。
 */
import { computed, ref, watch } from 'vue';

import { Alert, Checkbox, Input, Modal } from 'ant-design-vue';

const props = withDefaults(
  defineProps<{
    confirmName?: string;
    danger?: boolean;
    disabled?: boolean;
    description?: string;
    okText?: string;
    onConfirm: () => Promise<boolean | undefined> | boolean | undefined;
    riskText?: string;
    title: string;
  }>(),
  {
    confirmName: '',
    danger: false,
    disabled: false,
    description: '',
    okText: '确认',
    riskText: '',
  },
);

const open = defineModel<boolean>('open', { default: false });
const typedName = ref('');
const risk = ref(false);
const loading = ref(false);

watch(open, (v) => {
  if (v) {
    typedName.value = '';
    risk.value = false;
  }
});

const valid = computed(
  () =>
    !props.disabled &&
    (!props.confirmName || typedName.value === props.confirmName) &&
    (!props.riskText || risk.value),
);

async function ok() {
  if (!valid.value) return;
  loading.value = true;
  try {
    const done = await props.onConfirm();
    if (done) open.value = false;
  } finally {
    loading.value = false;
  }
}
</script>

<template>
  <Modal
    v-model:open="open"
    :confirm-loading="loading"
    destroy-on-close
    :ok-button-props="{ danger, disabled: !valid }"
    :ok-text="okText"
    :title="title"
    @ok="ok"
  >
    <Alert v-if="description" class="mb-3" :message="description" :type="danger ? 'warning' : 'info'" show-icon />
    <slot></slot>
    <div v-if="confirmName" class="mb-3">
      <div class="mb-1 text-sm">请输入 <b class="font-mono">{{ confirmName }}</b> 确认</div>
      <Input v-model:value="typedName" />
    </div>
    <Checkbox v-if="riskText" v-model:checked="risk">{{ riskText }}</Checkbox>
  </Modal>
</template>
