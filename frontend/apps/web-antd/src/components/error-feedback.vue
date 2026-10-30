<script lang="ts" setup>
import type { ErrorText } from '#/locales/errors';

import { TypographyText } from 'ant-design-vue';

defineProps<{ feedback: ErrorText | null; hideMessage?: boolean }>();
</script>

<template>
  <div v-if="feedback" class="min-w-0 break-words text-sm" role="alert">
    <div v-if="!hideMessage" class="font-medium">{{ feedback.text }}</div>
    <ul v-if="feedback.details.length" class="mt-1 max-h-48 space-y-1 overflow-y-auto">
      <li v-for="line in feedback.details" :key="line">{{ line }}</li>
    </ul>
    <div v-if="feedback.requestId" class="text-muted-foreground mt-1 text-xs">
      请求 ID：
      <TypographyText class="font-mono text-xs" :copyable="{ text: feedback.requestId }">
        {{ feedback.requestId }}
      </TypographyText>
    </div>
    <details v-if="feedback.code || feedback.taskCode || feedback.stage" class="text-muted-foreground mt-1 text-xs" @click.stop>
      <summary class="cursor-pointer">技术详情</summary>
      <div v-if="feedback.code">错误码：<span class="font-mono">{{ feedback.code }}</span></div>
      <div v-if="feedback.taskCode">任务记录错误码：<span class="font-mono">{{ feedback.taskCode }}</span></div>
      <div v-if="feedback.stage">阶段：<span class="font-mono">{{ feedback.stage }}</span></div>
    </details>
  </div>
</template>
