<script lang="ts" setup>
import type { Task } from '#/api/types';

import { computed, onMounted, ref } from 'vue';
import { useRoute } from 'vue-router';

import { Page } from '@vben/common-ui';

import { Button, Card, Descriptions, DescriptionsItem, Drawer, Input, Select, Space, Table } from 'ant-design-vue';

import { opsApi } from '#/api/backend';
import ConfirmAction from '#/components/confirm-action.vue';
import CursorPager from '#/components/cursor-pager.vue';
import ErrorFeedback from '#/components/error-feedback.vue';
import StatusTag from '#/components/status-tag.vue';
import TimeText from '#/components/time-text.vue';
import UserName from '#/components/user-name.vue';
import { errorToast, trackAccepted, useAction, useCursorList, useResource } from '#/composables/api';
import { ENUMS, TASK_NEXT_ACTIONS, TASK_TYPES } from '#/locales/enums';
import { describeTaskError } from '#/locales/errors';

const route = useRoute();
type TaskFilters = Record<'instance_id' | 'node_id' | 'status' | 'user_id', string | undefined>;
const filters = ref<TaskFilters>({
  instance_id: (route.query.instance_id as string) || undefined,
  node_id: (route.query.node_id as string) || undefined,
  status: (route.query.status as string) || undefined,
  user_id: (route.query.user_id as string) || undefined,
});
const list = useCursorList<Task>(opsApi.tasks, filters);

const statusOptions = Object.entries(ENUMS.task ?? {}).map(([value, l]) => ({ label: l.text, value }));

const columns = [
  { dataIndex: 'id', title: 'ID', width: 80 },
  { key: 'type', title: '类型' },
  { key: 'status', title: '状态', width: 120 },
  { dataIndex: 'phase', title: '阶段' },
  { key: 'target', title: '关联' },
  { key: 'user', title: '用户' },
  { key: 'created', title: '创建', width: 130 },
  { key: 'finished', title: '结束', width: 130 },
  { key: 'error', title: '失败原因 / 处理建议', width: 300 },
];

const selectedId = ref<null | string>(null);
const detail = useResource(() => (selectedId.value ? opsApi.task(selectedId.value) : null));
const drawerOpen = computed({
  get: () => !!selectedId.value,
  set: (v) => {
    if (!v) selectedId.value = null;
  },
});

async function openTask(id: string) {
  selectedId.value = id;
  try {
    await detail.reload();
  } catch (error) {
    errorToast(error);
  }
}

const reconcileOpen = ref(false);
const action = useAction();
async function reconcile() {
  const id = selectedId.value;
  if (!id) return false;
  const r = await action.run((key) => opsApi.reconcile(id, { ifMatch: detail.etag.value, key }), {
    onConflict: () => detail.reload(),
    success: '已请求核对',
  });
  if (!r) return false;
  trackAccepted(r.data, '任务核对', () => list.load());
  await detail.reload();
  return true;
}

onMounted(() => list.load());
</script>

<template>
  <Page title="任务">
    <Card size="small">
      <Space class="mb-3" wrap>
        <Select
          v-model:value="filters.status"
          allow-clear
          class="w-36"
          :options="statusOptions"
          placeholder="状态"
        />
        <Input v-model:value="filters.instance_id" allow-clear class="w-36" placeholder="实例 ID" />
        <Input v-model:value="filters.node_id" allow-clear class="w-32" placeholder="节点 ID" />
        <Input v-model:value="filters.user_id" allow-clear class="w-32" placeholder="用户 ID" />
      </Space>
      <Table
        :columns="columns"
        :data-source="list.items.value"
        :loading="list.loading.value"
        :pagination="false"
        row-key="id"
        size="small"
        :custom-row="(r: Task) => ({ onClick: () => openTask(r.id), style: 'cursor:pointer' })"
      >
        <template #bodyCell="{ column, record }">
          <template v-if="column.key === 'type'">{{ TASK_TYPES[record.type] ?? record.type }}</template>
          <template v-else-if="column.key === 'status'"><StatusTag kind="task" :value="record.status" /></template>
          <template v-else-if="column.key === 'target'">
            <RouterLink v-if="record.instance_id" :to="`/admin/instances/${record.instance_id}`" @click.stop>
              实例 #{{ record.instance_id }}
            </RouterLink>
            <RouterLink v-else-if="record.node_id" :to="`/admin/nodes/${record.node_id}`" @click.stop>
              节点 #{{ record.node_id }}
            </RouterLink>
            <span v-else>—</span>
          </template>
          <template v-else-if="column.key === 'user'"><UserName :id="record.user_id" /></template>
          <template v-else-if="column.key === 'created'"><TimeText :value="record.created_at" /></template>
          <template v-else-if="column.key === 'finished'"><TimeText :value="record.finished_at" /></template>
          <template v-else-if="column.key === 'error'">
            <ErrorFeedback :feedback="describeTaskError(record, list.requestId.value)" />
          </template>
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

    <Drawer v-model:open="drawerOpen" :title="`任务 #${selectedId ?? ''}`" :width="520">
      <template v-if="detail.data.value" #extra>
        <Button
          v-if="detail.data.value.status === 'unknown'"
          type="primary"
          @click="reconcileOpen = true"
        >
          请求核对
        </Button>
      </template>
      <template v-if="detail.data.value">
        <Descriptions bordered :column="1" size="small">
          <DescriptionsItem label="类型">{{ TASK_TYPES[detail.data.value.type] ?? detail.data.value.type }}</DescriptionsItem>
          <DescriptionsItem label="状态"><StatusTag kind="task" :value="detail.data.value.status" /></DescriptionsItem>
          <DescriptionsItem label="阶段"><span class="font-mono">{{ detail.data.value.phase || '—' }}</span></DescriptionsItem>
          <DescriptionsItem label="下一步">
            {{ TASK_NEXT_ACTIONS[detail.data.value.next_action] ?? (detail.data.value.next_action ? '请查看任务状态与处理建议' : '—') }}
            <details v-if="detail.data.value.next_action" class="text-muted-foreground mt-1 text-xs">
              <summary class="cursor-pointer">技术详情</summary>
              <span class="font-mono">{{ detail.data.value.next_action }}</span>
            </details>
          </DescriptionsItem>
          <DescriptionsItem label="创建"><TimeText :value="detail.data.value.created_at" /></DescriptionsItem>
          <DescriptionsItem label="开始"><TimeText :value="detail.data.value.started_at" /></DescriptionsItem>
          <DescriptionsItem label="结束"><TimeText :value="detail.data.value.finished_at" /></DescriptionsItem>
        </Descriptions>
        <ErrorFeedback class="mt-3" :feedback="describeTaskError(detail.data.value, detail.requestId.value)" />
        <div v-if="detail.data.value.status === 'failed'" class="text-muted-foreground mt-3 text-sm">
          任务已失败，不会自动重试。请先修正失败原因；是否需要重新提交应以资源实际状态为准。
        </div>
        <div v-if="detail.data.value.status === 'unknown'" class="mt-3 text-sm text-orange-500">
          外部操作结果未确认。核对只重查实际状态，不会重放操作；后台不提供"手动标记成功"。
        </div>
        <h4 class="mb-2 mt-4 font-medium">步骤</h4>
        <Table
          :columns="[
            { dataIndex: 'name', title: '名称' },
            { key: 'status', title: '状态' },
            { key: 'started', title: '开始' },
            { key: 'finished', title: '结束' },
            { key: 'error', title: '失败原因 / 处理建议' },
          ]"
          :data-source="detail.data.value.steps"
          :pagination="false"
          row-key="name"
          size="small"
        >
          <template #bodyCell="{ column, record }">
            <template v-if="column.key === 'status'"><StatusTag kind="task" :value="record.status" /></template>
            <template v-else-if="column.key === 'started'"><TimeText :value="record.started_at" /></template>
            <template v-else-if="column.key === 'finished'"><TimeText :value="record.finished_at" /></template>
            <template v-else-if="column.key === 'error'">
              <ErrorFeedback :feedback="describeTaskError(record, detail.requestId.value)" />
            </template>
          </template>
        </Table>
      </template>
    </Drawer>

    <ConfirmAction
      v-model:open="reconcileOpen"
      description="重新查询节点上的实际状态，并根据结果结束该任务。不会重新执行原操作。"
      :on-confirm="reconcile"
      title="请求核对任务"
    />
  </Page>
</template>
