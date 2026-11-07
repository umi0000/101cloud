<script lang="ts" setup>
import { computed, ref, watch } from 'vue';
import { useRouter } from 'vue-router';

import { Badge, Button, Drawer, Empty, Tooltip } from 'ant-design-vue';

import { TASK_TYPES } from '#/locales/enums';
import { describeTaskError } from '#/locales/errors';
import { useTaskStore } from '#/store/tasks';
import { useAuthStore } from '#/store/auth';
import { creationState } from '#/utils/creation-progress';

import TaskForceStopConfirm from './task-force-stop-confirm.vue';
import ErrorFeedback from './error-feedback.vue';
import StatusTag from './status-tag.vue';
import TimeText from './time-text.vue';

const tasks = useTaskStore();
const auth = useAuthStore();
const router = useRouter();
const stopOpen = ref(false);
const stopId = ref<null | string>(null);
const selected = computed(() => tasks.tracked.find((task) => task.taskId === stopId.value));

function canStop(status: string) {
  return !['cancelled', 'failed', 'succeeded'].includes(status);
}

function openStop(taskId: string) {
  stopId.value = taskId;
  stopOpen.value = true;
}

watch(() => [auth.me?.user.id, auth.me?.user.role], () => {
  stopOpen.value = false;
  stopId.value = null;
});
watch(() => selected.value?.task?.status, (status) => {
  if (!status || !canStop(status)) stopOpen.value = false;
});

function openInitialization(taskId: string) {
  tasks.open = false;
  void router.push({ name: 'MemberInstanceInitialization', params: { taskId } });
}
</script>

<template>
  <Tooltip title="任务中心">
    <Badge :count="tasks.active.length" :offset="[-4, 4]" size="small">
      <Button aria-label="任务中心" type="text" @click="tasks.open = true">
        <span class="icon-[lucide--list-checks] size-4"></span>
      </Button>
    </Badge>
  </Tooltip>
  <Drawer v-model:open="tasks.open" title="任务中心" width="min(460px, 100vw)">
    <template #extra>
      <Button size="small" @click="tasks.clearFinished()">清除已完成</Button>
    </template>
    <Empty v-if="tasks.tracked.length === 0" description="本次会话没有发起任务" />
    <div v-for="t in tasks.tracked" :key="t.taskId" class="border-border mb-3 rounded border p-3">
      <div class="flex flex-wrap items-center justify-between gap-2">
        <span class="min-w-0 break-words font-medium">{{ (t.task && TASK_TYPES[t.task.type]) || t.label }}</span>
        <span v-if="t.task?.type === 'instance.create' && t.task.status === 'succeeded' && creationState(t.task) !== 'succeeded'" class="text-muted-foreground text-xs">初始化待确认</span>
        <StatusTag v-else-if="t.task" kind="task" :value="t.task.status" />
        <span v-else class="text-muted-foreground text-xs">查询中</span>
      </div>
      <div class="text-muted-foreground mt-1 break-all text-xs">
        任务 <span class="font-mono">#{{ t.taskId }}</span>
        <template v-if="t.task">
          · 阶段 <span class="font-mono">{{ t.task.phase || '—' }}</span> · 创建
          <TimeText :value="t.task.created_at" />
        </template>
      </div>
      <div v-if="t.task?.type === 'image.prepare'" class="mt-2 text-xs">
        {{ t.label }}<span v-if="t.task.node_id" class="ml-2">节点 #{{ t.task.node_id }}</span>
        <div v-if="typeof t.task.result?.progress === 'string'" class="mt-1 font-mono">{{ t.task.result.progress }}</div>
      </div>
      <div v-if="t.task?.status === 'unknown'" class="mt-2 text-xs text-orange-500">
        结果待核对，不会自动重新发起操作。
        <template v-if="auth.isAdmin">可在“管理 / 任务”中请求核对。</template>
        <template v-else>请联系管理员核对，勿重复创建。</template>
      </div>
      <div v-if="t.task?.force_stop" class="mt-2 text-xs" role="status">
        <p class="mb-1">任务编排已强制停止，历史记录保留。</p>
        <p v-if="t.task.force_stop.reconciliation_completed" class="text-muted-foreground mb-0">外部结果核对已完成；资源与计费仍以实际记录为准。</p>
        <p v-else-if="t.task.force_stop.requires_reconciliation" class="mb-0 text-orange-500">已发出的外部操作不保证停止，资源与计费不视为已释放。请联系管理员核对，勿重复提交原操作。</p>
        <p v-else class="text-muted-foreground mb-0">服务端确认尚未派发外部操作；资源与计费以实际记录为准。</p>
      </div>
      <div v-if="t.stop?.feedback" class="mt-2" role="alert"><ErrorFeedback :feedback="t.stop.feedback" /></div>
      <div v-if="t.stop?.uncertain" class="mt-2 text-xs text-orange-500" role="status">强停提交结果尚未确认；不会生成新请求。请重试原请求核对结果。</div>
      <ErrorFeedback v-if="t.task" class="mt-2" :feedback="describeTaskError(t.task, t.requestId)" />
      <div v-if="t.task?.status === 'failed'" class="text-muted-foreground mt-2 text-xs">
        {{ t.task.creation_cleanup ? '任务已失败，不会自动重新创建。' : '任务已失败，不会自动重试。请先处理原因，再决定是否重新提交。' }}
      </div>
      <div class="mt-3 flex flex-wrap gap-2">
        <Button v-if="!auth.isAdmin && t.task?.type === 'instance.create'" size="small" @click="openInitialization(t.taskId)">查看初始化</Button>
        <Button v-if="t.task && canStop(t.task.status)" danger size="small" :loading="t.stop?.pending" :disabled="t.stop?.pending || (t.task.can_force_stop === false && !t.stop?.uncertain)" @click="openStop(t.taskId)">{{ t.stop?.uncertain ? '重试原强停请求' : '强制停止' }}</Button>
      </div>
      <p v-if="t.task && canStop(t.task.status) && t.task.can_force_stop === false && !t.stop?.uncertain" class="text-muted-foreground mt-2 text-xs">{{ t.task.force_stop_blocked_reason === 'task_stop_not_allowed' ? '系统执行任务仅管理员可强停' : '当前账号无权强停此任务，请联系管理员核对。' }}</p>
      <div v-if="t.task?.steps?.length" class="mt-2 space-y-1">
        <div v-for="s in t.task.steps" :key="s.name" class="text-xs">
          <div class="flex items-center justify-between">
            <span class="font-mono">{{ s.name }}</span>
            <StatusTag kind="task" :value="s.status" />
          </div>
          <ErrorFeedback :feedback="describeTaskError(s, t.requestId)" />
        </div>
      </div>
    </div>
  </Drawer>
  <TaskForceStopConfirm
    v-if="selected"
    :key="selected.taskId"
    v-model:open="stopOpen"
    :disabled="!selected.task || !canStop(selected.task.status) || (selected.task.can_force_stop === false && !selected.stop?.uncertain)"
    :feedback="selected.stop?.feedback"
    :label="selected.task ? TASK_TYPES[selected.task.type] || selected.label : selected.label"
    :pending="selected.stop?.pending"
    :task-id="selected.taskId"
    :uncertain="selected.stop?.uncertain"
    :on-confirm="() => tasks.forceStop(selected!.taskId)"
  />
</template>
