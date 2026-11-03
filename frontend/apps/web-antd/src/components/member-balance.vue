<script lang="ts" setup>
import type { CreditAccount, CreditEnforcement } from '#/api/types';
import type { ErrorText } from '#/locales/errors';

import { computed, onBeforeUnmount, ref, shallowRef, watch } from 'vue';

import { useAccessStore } from '@vben/stores';

import { Alert, Button, Popover } from 'ant-design-vue';

import { memberApi } from '#/api/backend';
import { usePolling } from '#/composables/api';
import { describeError } from '#/locales/errors';
import { useAuthStore } from '#/store';
import { useTaskStore } from '#/store/tasks';
import { formatPt } from '#/utils/format';

import ErrorFeedback from './error-feedback.vue';
import StatusTag from './status-tag.vue';
import TimeText from './time-text.vue';

const authStore = useAuthStore();
const accessStore = useAccessStore();
const tasks = useTaskStore();
const account = shallowRef<CreditAccount | null>(null);
const feedback = shallowRef<ErrorText | null>(null);
const loading = ref(false);
const open = ref(false);
let generation = 0;
let disposed = false;

const memberUserId = computed(() =>
  authStore.me?.user.role === 'user' && !accessStore.loginExpired
    ? authStore.me.user.id
    : null,
);
const hasEnforcementWarning = computed(() =>
  account.value?.enforcements.some((event) => ['grace', 'stopping', 'unknown'].includes(event.status)) ?? false,
);

function enforcementMessage(status: CreditEnforcement['status']): string {
  switch (status) {
    case 'grace': {
      return '额度已进入宽限期，请留意服务端停机期限。';
    }
    case 'stopping': {
      return '额度停机处理中，实例状态以服务端更新为准。';
    }
    case 'unknown': {
      return '额度停机状态待核对，请联系管理员。';
    }
    case 'completed': {
      return '额度停机事件已完成，请核对对应实例的状态。';
    }
    case 'cancelled': {
      return '额度停机事件已取消。';
    }
    default: {
      return '';
    }
  }
}

async function refresh() {
  const userId = memberUserId.value;
  if (disposed || !userId || loading.value) return;
  const mine = generation;
  loading.value = true;
  try {
    const result = await memberApi.creditAccount();
    if (disposed || mine !== generation || memberUserId.value !== userId) return;
    account.value = result.data;
    feedback.value = null;
  } catch (error) {
    if (disposed || mine !== generation || memberUserId.value !== userId) return;
    feedback.value = describeError(error);
  } finally {
    if (!disposed && mine === generation) loading.value = false;
  }
}

watch(memberUserId, (userId) => {
  generation += 1;
  account.value = null;
  feedback.value = null;
  loading.value = false;
  open.value = false;
  if (userId) void refresh();
}, { immediate: true, flush: 'sync' });

usePolling(refresh, () => 30_000);
watch(
  () => tasks.tracked.map((item) => `${item.taskId}:${item.task?.status ?? 'accepted'}`).join('|'),
  () => void refresh(),
);
onBeforeUnmount(() => {
  disposed = true;
  generation += 1;
  account.value = null;
  feedback.value = null;
  open.value = false;
});
</script>

<template>
  <Popover v-if="memberUserId" v-model:open="open" placement="bottomRight" trigger="click">
    <template #content>
      <div class="w-72 max-w-[calc(100vw-2rem)] space-y-3" :aria-busy="loading">
        <div class="flex items-center justify-between gap-3">
          <span class="font-medium">额度详情</span>
          <Button :loading="loading" size="small" @click="refresh">刷新</Button>
        </div>
        <dl class="m-0 space-y-2">
          <div class="flex items-start justify-between gap-4">
            <dt class="text-muted-foreground shrink-0">可用额度（净额）</dt>
            <dd class="m-0 min-w-0 break-all text-right font-mono font-medium">{{ formatPt(account?.available_pt) }} Pt</dd>
          </div>
          <div class="flex items-start justify-between gap-4">
            <dt class="text-muted-foreground shrink-0">剩余额度（净额）</dt>
            <dd class="m-0 min-w-0 break-all text-right font-mono">{{ formatPt(account?.remaining_pt) }} Pt</dd>
          </div>
          <div class="flex items-start justify-between gap-4">
            <dt class="text-muted-foreground shrink-0">已预留额度</dt>
            <dd class="m-0 min-w-0 break-all text-right font-mono">{{ formatPt(account?.reserved_pt) }} Pt</dd>
          </div>
          <div class="flex items-start justify-between gap-4">
            <dt class="text-muted-foreground shrink-0">未偿欠款（总额）</dt>
            <dd class="m-0 min-w-0 break-all text-right font-mono">{{ formatPt(account?.outstanding_debt_pt) }} Pt</dd>
          </div>
        </dl>
        <p class="text-muted-foreground m-0 text-xs">剩余与可用已扣除欠款；可用还扣除预留，请勿再次扣除未偿欠款。欠款未还清时净余额可能为负数，无待处理任务不代表欠款已还清。</p>
        <Alert
          v-if="account?.settlement_pending"
          description="已入账额度正在抵扣历史欠费，不代表偿债已完成；未偿欠款总额可能暂未下降。净余额已反映入账金额，可刷新查看最新进展。"
          message="历史欠费正在后台分批抵扣"
          role="status"
          show-icon
          type="info"
        />
        <Alert v-if="feedback" show-icon type="error" message="额度更新失败">
          <template #description>
            <p class="mb-1">{{ account ? '当前保留上次成功获取的额度。' : '暂时无法获取额度，请刷新重试。' }}</p>
            <ErrorFeedback :feedback="feedback" />
          </template>
        </Alert>
        <Alert
          v-for="event in account?.enforcements ?? []"
          :key="event.id"
          :message="enforcementMessage(event.status)"
          show-icon
          :type="['grace', 'stopping', 'unknown'].includes(event.status) ? 'warning' : 'info'"
        >
          <template #description>
            <div class="flex flex-wrap items-center gap-1">
              <span class="font-mono text-xs">#{{ event.id }}</span>
              <StatusTag kind="enforcement" :value="event.status" />
              <span>政策 #{{ event.policy_version_id }}</span>
              <span>停机期限</span>
              <TimeText :value="event.shutdown_deadline" />
              <span v-if="event.instance_ids.length">实例 #{{ event.instance_ids.join('、#') }}</span>
            </div>
          </template>
        </Alert>
        <div class="text-muted-foreground text-xs">
          <span v-if="loading" role="status">正在更新额度…</span>
          <template v-else-if="account">服务端时间 <TimeText :value="account.server_time" /></template>
          <div>页面可见时每 30 秒自动更新。</div>
        </div>
      </div>
    </template>
    <Button
      type="text"
      :aria-expanded="open"
      :aria-label="`可用额度 ${formatPt(account?.available_pt)} Pt，点击查看额度详情${account?.settlement_pending ? '，历史欠费正在后台分批抵扣' : ''}${feedback ? '，更新失败' : ''}${hasEnforcementWarning ? '，存在额度停机提示' : ''}`"
    >
      <span class="inline-flex min-w-0 items-center gap-1.5">
        <span class="text-muted-foreground hidden sm:inline">可用额度</span>
        <span class="max-w-[7rem] truncate font-mono sm:max-w-[12rem]">{{ formatPt(account?.available_pt) }}</span>
        <span>Pt</span>
        <span v-if="feedback" class="text-destructive inline-flex items-center gap-1" role="status">
          <span aria-hidden="true" class="icon-[lucide--circle-alert] size-4"></span>
          <span class="hidden text-xs sm:inline">更新失败</span>
        </span>
        <span v-else-if="hasEnforcementWarning" aria-hidden="true" class="text-destructive icon-[lucide--triangle-alert] size-4"></span>
        <span aria-hidden="true" class="text-muted-foreground icon-[lucide--chevron-down] size-3"></span>
      </span>
    </Button>
  </Popover>
</template>
