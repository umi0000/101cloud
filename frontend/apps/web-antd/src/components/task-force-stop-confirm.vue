<script lang="ts" setup>
import type { ErrorText } from '#/locales/errors';

import ConfirmAction from './confirm-action.vue';
import ErrorFeedback from './error-feedback.vue';

withDefaults(defineProps<{
  disabled?: boolean;
  feedback?: ErrorText | null;
  label: string;
  onConfirm: () => Promise<boolean>;
  pending?: boolean;
  taskId: string;
  uncertain?: boolean;
}>(), { disabled: false, feedback: null, pending: false, uncertain: false });
const open = defineModel<boolean>('open', { default: false });
</script>

<template>
  <ConfirmAction
    v-model:open="open"
    danger
    :disabled="disabled || pending"
    title="强制停止任务编排"
    :ok-text="uncertain ? '重试原强停请求' : '确认强制停止'"
    description="仅停止此任务的后续编排，不保证已经发出的外部操作停止。不会删除任务历史，也不代表实例、IP、GPU 或计费预留已经释放。"
    risk-text="我已了解：外部结果可能仍待核对，强停后不会自动重放原操作；资源和计费须以实际核对结果为准。"
    :on-confirm="onConfirm"
  >
    <p class="mb-3 break-all text-sm">任务 <span class="font-mono">#{{ taskId }}</span> · {{ label }}</p>
    <p v-if="uncertain" class="mb-3 text-sm text-orange-500" role="status">上次强停提交结果尚未确认。本次将复用原 If-Match 与幂等键，只重试原强停请求，不重新执行任务。</p>
    <div v-if="feedback" class="mb-3" role="alert"><ErrorFeedback :feedback="feedback" /></div>
  </ConfirmAction>
</template>
