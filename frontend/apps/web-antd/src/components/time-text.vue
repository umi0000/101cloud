<script lang="ts" setup>
import { Tooltip } from 'ant-design-vue';

import { formatFullTime, formatTime } from '#/utils/format';

defineProps<{ value?: null | string }>();
</script>

<template>
  <Tooltip v-if="value" :title="formatFullTime(value)">
    <span class="tabular-nums">{{ formatTime(value) }}</span>
  </Tooltip>
  <span v-else class="text-muted-foreground">—</span>
</template>
