<script lang="ts" setup>
/** 任务标签页：GET /admin/tasks?instance_id=，游标分页，展开行显示步骤。 */
import type { Task } from '#/api/types';

import { onMounted, ref } from 'vue';

import { Card, Table } from 'ant-design-vue';

import { opsApi } from '#/api/backend';
import CursorPager from '#/components/cursor-pager.vue';
import ErrorFeedback from '#/components/error-feedback.vue';
import StatusTag from '#/components/status-tag.vue';
import TimeText from '#/components/time-text.vue';
import { useCursorList } from '#/composables/api';
import { TASK_TYPES } from '#/locales/enums';
import { describeTaskError } from '#/locales/errors';

const props = defineProps<{ instanceId: string }>();

const filters = ref<Record<'instance_id', string>>({ instance_id: props.instanceId });
const list = useCursorList<Task>(opsApi.tasks, filters);

const columns = [
  { dataIndex: 'id', title: 'ID', width: 80 },
  { key: 'type', title: '类型' },
  { key: 'status', title: '状态', width: 120 },
  { dataIndex: 'phase', title: '阶段' },
  { key: 'created', title: '创建', width: 130 },
  { key: 'finished', title: '结束', width: 130 },
  { key: 'error', title: '失败原因 / 处理建议', width: 300 },
];
const stepColumns = [
  { dataIndex: 'name', title: '步骤' },
  { key: 'status', title: '状态' },
  { key: 'started', title: '开始' },
  { key: 'finished', title: '结束' },
  { key: 'error', title: '失败原因 / 处理建议' },
];

onMounted(() => list.load());
</script>

<template>
  <Card size="small">
    <Table
      :columns="columns"
      :data-source="list.items.value"
      :loading="list.loading.value"
      :pagination="false"
      row-key="id"
      size="small"
    >
      <template #bodyCell="{ column, record }">
        <template v-if="column.key === 'type'">{{ TASK_TYPES[record.type] ?? record.type }}</template>
        <template v-else-if="column.key === 'status'"><StatusTag kind="task" :value="record.status" /></template>
        <template v-else-if="column.key === 'created'"><TimeText :value="record.created_at" /></template>
        <template v-else-if="column.key === 'finished'"><TimeText :value="record.finished_at" /></template>
        <template v-else-if="column.key === 'error'">
          <ErrorFeedback :feedback="describeTaskError(record, list.requestId.value)" />
        </template>
      </template>
      <template #expandedRowRender="{ record }">
        <div v-if="record.status === 'unknown'" class="mb-2 text-sm text-orange-500">
          结果未确认，不会自动重放操作。请在任务详情请求管理员核对。
        </div>
        <div v-else-if="record.status === 'failed'" class="text-muted-foreground mb-2 text-sm">
          任务已失败，不提供盲目重试。请先处理原因并确认资源实际状态。
        </div>
        <Table :columns="stepColumns" :data-source="record.steps" :pagination="false" row-key="name" size="small">
          <template #bodyCell="{ column, record: step }">
            <template v-if="column.key === 'status'"><StatusTag kind="task" :value="step.status" /></template>
            <template v-else-if="column.key === 'started'"><TimeText :value="step.started_at" /></template>
            <template v-else-if="column.key === 'finished'"><TimeText :value="step.finished_at" /></template>
            <template v-else-if="column.key === 'error'">
              <ErrorFeedback :feedback="describeTaskError(step, list.requestId.value)" />
            </template>
          </template>
        </Table>
      </template>
    </Table>
    <CursorPager
      :has-next="list.hasNext()"
      :has-prev="list.hasPrev()"
      :loading="list.loading.value"
      :page="list.page()"
      @next="list.nextPage"
      @prev="list.prevPage"
      @refresh="list.load"
    />
  </Card>
</template>
