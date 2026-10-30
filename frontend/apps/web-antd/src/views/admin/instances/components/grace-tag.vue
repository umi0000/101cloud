<script lang="ts" setup>
/** 额度宽限标签：grace/stopping/unknown 时显示红色状态与 shutdown_deadline 倒计时。 */
import type { Instance } from '#/api/types';

import { computed } from 'vue';

import { Tag, Tooltip } from 'ant-design-vue';

import { enumLabel } from '#/locales/enums';
import { formatFullTime } from '#/utils/format';

import { formatCountdown, useNow } from './shared';

const props = defineProps<{ enforcement: Instance['credit_enforcement'] }>();
const now = useNow();

const active = computed(() => ['grace', 'stopping', 'unknown'].includes(props.enforcement.status));
const text = computed(() => {
  const label = enumLabel('enforcement', props.enforcement.status).text;
  const deadline = props.enforcement.shutdown_deadline ? Date.parse(props.enforcement.shutdown_deadline) : Number.NaN;
  return Number.isNaN(deadline) ? label : `${label} ${formatCountdown(deadline - now.value)}`;
});
</script>

<template>
  <Tooltip
    v-if="active"
    :title="enforcement.shutdown_deadline ? `停机期限 ${formatFullTime(enforcement.shutdown_deadline)}` : undefined"
  >
    <Tag class="m-0 tabular-nums" color="error">{{ text }}</Tag>
  </Tooltip>
</template>
