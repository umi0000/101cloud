<script lang="ts" setup>
import { Badge, Button, Drawer, Empty, Tooltip } from 'ant-design-vue';

import { TASK_TYPES } from '#/locales/enums';
import { describeTaskError } from '#/locales/errors';
import { useTaskStore } from '#/store/tasks';

import ErrorFeedback from './error-feedback.vue';
import StatusTag from './status-tag.vue';
import TimeText from './time-text.vue';

const tasks = useTaskStore();
</script>

<template>
  <Tooltip title="任务中心">
    <Badge :count="tasks.active.length" :offset="[-4, 4]" size="small">
      <Button type="text" @click="tasks.open = true">
        <span class="icon-[lucide--list-checks] size-4"></span>
      </Button>
    </Badge>
  </Tooltip>
  <Drawer v-model:open="tasks.open" title="任务中心" :width="460">
    <template #extra>
      <Button size="small" @click="tasks.clearFinished()">清除已完成</Button>
    </template>
    <Empty v-if="tasks.tracked.length === 0" description="本次会话没有发起任务" />
    <div v-for="t in tasks.tracked" :key="t.taskId" class="border-border mb-3 rounded border p-3">
      <div class="flex items-center justify-between">
        <span class="font-medium">{{ (t.task && TASK_TYPES[t.task.type]) || t.label }}</span>
        <StatusTag v-if="t.task" kind="task" :value="t.task.status" />
        <span v-else class="text-muted-foreground text-xs">查询中</span>
      </div>
      <div class="text-muted-foreground mt-1 text-xs">
        任务 <span class="font-mono">#{{ t.taskId }}</span>
        <template v-if="t.task">
          · 阶段 <span class="font-mono">{{ t.task.phase || '—' }}</span> · 创建
          <TimeText :value="t.task.created_at" />
        </template>
      </div>
      <div v-if="t.task?.status === 'unknown'" class="mt-2 text-xs text-orange-500">
        结果待核对，不会自动重新发起操作。可在"管理 / 任务"中请求核对。
      </div>
      <ErrorFeedback v-if="t.task" class="mt-2" :feedback="describeTaskError(t.task, t.requestId)" />
      <div v-if="t.task?.status === 'failed'" class="text-muted-foreground mt-2 text-xs">
        任务已失败，不会自动重试。请先处理原因，再决定是否重新提交。
      </div>
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
</template>
