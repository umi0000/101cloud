<script lang="ts" setup>
import type { Task } from '#/api/types';

import { computed, onBeforeUnmount, onMounted, ref, shallowRef, watch } from 'vue';
import { useRoute } from 'vue-router';

import { Page } from '@vben/common-ui';

import { Button, Card, Descriptions, DescriptionsItem, Drawer, Input, Select, Table } from 'ant-design-vue';

import { opsApi } from '#/api/backend';
import { TaskStatusSchema } from '#/api/task-schema';
import ConfirmAction from '#/components/confirm-action.vue';
import CursorPager from '#/components/cursor-pager.vue';
import ErrorFeedback from '#/components/error-feedback.vue';
import StatusTag from '#/components/status-tag.vue';
import TimeText from '#/components/time-text.vue';
import TaskForceStopConfirm from '#/components/task-force-stop-confirm.vue';
import UserName from '#/components/user-name.vue';
import { errorToast, trackAccepted, useAction, useCursorList } from '#/composables/api';
import { ENUMS, TASK_NEXT_ACTIONS, TASK_TYPES } from '#/locales/enums';
import { describeError, describeTaskError } from '#/locales/errors';
import { useAuthStore } from '#/store/auth';
import { useTaskStore } from '#/store/tasks';

const route = useRoute();
const auth = useAuthStore();
const tasks = useTaskStore();
const actor = computed(() => `${auth.me?.user.id ?? ''}:${auth.me?.user.role ?? ''}`);
let detailSequence = 0;
let disposed = false;
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
  { key: 'actions', title: '操作', width: 160, fixed: 'right' as const },
];

const selectedId = ref<null | string>(null);
const detail = {
  data: shallowRef<Task | null>(null),
  etag: ref<string | null>(null),
  error: shallowRef<unknown>(null),
  loading: ref(false),
  requestId: ref(''),
  reload: reloadDetail,
};
const stopOpen = ref(false);
const stopState = computed(() => tasks.tracked.find((task) => task.taskId === selectedId.value)?.stop);

function canStop(task: unknown) {
  if (typeof task !== 'object' || task === null || !('status' in task)) return false;
  const status = TaskStatusSchema.safeParse(task.status);
  return status.success && !['cancelled', 'failed', 'succeeded'].includes(status.data);
}

async function reloadDetail() {
  const id = selectedId.value;
  if (!id || disposed) return false;
  const mine = ++detailSequence;
  const owner = actor.value;
  detail.loading.value = true;
  try {
    const response = await opsApi.task(id);
    if (disposed || mine !== detailSequence || actor.value !== owner || selectedId.value !== id) return false;
    detail.data.value = response.data;
    detail.etag.value = response.etag;
    detail.requestId.value = response.requestId;
    detail.error.value = null;
    return true;
  } catch (failure) {
    if (disposed || mine !== detailSequence || actor.value !== owner || selectedId.value !== id) return false;
    detail.error.value = failure;
    errorToast(failure);
    return false;
  } finally {
    if (!disposed && mine === detailSequence && actor.value === owner) detail.loading.value = false;
  }
}
const drawerOpen = computed({
  get: () => !!selectedId.value,
  set: (v) => {
    if (!v) {
      selectedId.value = null;
      stopOpen.value = false;
      reconcileOpen.value = false;
      detailSequence++;
      detail.loading.value = false;
    }
  },
});

async function openTask(id: string) {
  stopOpen.value = false;
  reconcileOpen.value = false;
  selectedId.value = id;
  detail.data.value = null;
  detail.etag.value = null;
  detail.error.value = null;
  return detail.reload();
}

async function prepareStop(id: string) {
  if (await openTask(id) && detail.data.value && selectedId.value === id && canStop(detail.data.value)) stopOpen.value = true;
}

async function forceStop() {
  const task = detail.data.value;
  const id = selectedId.value;
  if (!task || !id || task.id !== id || detail.loading.value || !canStop(task)) return false;
  const owner = actor.value;
  return tasks.forceStop(id, {
    task,
    etag: detail.etag.value,
    onDone: () => {
      if (disposed || actor.value !== owner) return;
      void list.load();
      if (selectedId.value !== id) return;
      const updated = tasks.tracked.find((item) => item.taskId === id);
      if (updated?.task) {
        detail.data.value = updated.task;
        detail.etag.value = updated.etag;
      }
      void detail.reload();
    },
  });
}

watch(actor, () => {
  detailSequence++;
  selectedId.value = null;
  detail.data.value = null;
  detail.etag.value = null;
  detail.error.value = null;
  detail.loading.value = false;
  stopOpen.value = false;
  reconcileOpen.value = false;
});
watch(() => detail.data.value?.status, (status) => {
  if (!status || ['cancelled', 'failed', 'succeeded'].includes(status)) stopOpen.value = false;
});
onBeforeUnmount(() => { disposed = true; detailSequence++; });

const reconcileOpen = ref(false);
const action = useAction();
async function reconcile() {
  const id = selectedId.value;
  if (!id) return false;
  const r = await action.run((key) => opsApi.reconcile(id, { ifMatch: detail.etag.value, key }), {
    onConflict: async () => { await detail.reload(); },
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
      <div class="mb-3 flex flex-wrap items-center gap-2">
        <Select
          v-model:value="filters.status"
          allow-clear
          class="!w-36"
          :options="statusOptions"
          placeholder="状态"
        />
        <Input v-model:value="filters.instance_id" allow-clear class="!w-36" placeholder="实例 ID" />
        <Input v-model:value="filters.node_id" allow-clear class="!w-32" placeholder="节点 ID" />
        <Input v-model:value="filters.user_id" allow-clear class="!w-32" placeholder="用户 ID" />
      </div>
      <Table
        :columns="columns"
        :data-source="list.items.value"
        :loading="list.loading.value"
        :pagination="false"
        :scroll="{ x: 'max-content' }"
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
          <template v-else-if="column.key === 'actions'">
            <Button v-if="canStop(record)" danger size="small" :loading="selectedId === record.id && stopState?.pending" :disabled="record.can_force_stop === false || (selectedId === record.id && stopState?.pending)" @click.stop="prepareStop(record.id)">强制停止</Button>
            <p v-if="canStop(record) && record.can_force_stop === false" class="text-muted-foreground mb-0 mt-1 text-xs">{{ record.force_stop_blocked_reason === 'task_stop_not_allowed' ? '系统执行任务仅管理员可强停' : '当前账号无权强停此任务，请核对权限。' }}</p>
            <span v-if="!canStop(record)" class="text-muted-foreground text-xs">历史记录</span>
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

    <Drawer v-model:open="drawerOpen" :title="`任务 #${selectedId ?? ''}`" width="min(520px, 100vw)">
      <template v-if="detail.data.value" #extra>
        <Button v-if="canStop(detail.data.value)" danger class="mr-2" :loading="stopState?.pending" :disabled="detail.loading.value || stopState?.pending || (detail.data.value.can_force_stop === false && !stopState?.uncertain)" @click="prepareStop(detail.data.value.id)">{{ stopState?.uncertain ? '重试原强停请求' : '强制停止' }}</Button>
        <Button
          v-if="detail.data.value.status === 'unknown' || detail.data.value.force_stop?.requires_reconciliation"
          type="primary"
          :disabled="stopState?.pending || stopState?.uncertain || detail.loading.value"
          @click="reconcileOpen = true"
        >
          请求核对
        </Button>
      </template>
      <p v-if="detail.loading.value" class="text-muted-foreground text-sm" role="status">正在读取任务详情与版本…</p>
      <div v-if="detail.error.value" class="mb-3" role="alert"><ErrorFeedback :feedback="describeError(detail.error.value)" /><Button size="small" :loading="detail.loading.value" @click="detail.reload">刷新任务详情</Button></div>
      <template v-if="detail.data.value">
        <p v-if="canStop(detail.data.value) && detail.data.value.can_force_stop === false" class="text-muted-foreground mb-3 text-sm">{{ detail.data.value.force_stop_blocked_reason === 'task_stop_not_allowed' ? '系统执行任务仅管理员可强停' : '当前账号无权强停此任务，请核对权限。' }}</p>
        <div v-if="stopState?.feedback" class="mb-3" role="alert"><ErrorFeedback :feedback="stopState.feedback" /></div>
        <p v-if="stopState?.uncertain" class="mb-3 text-sm text-orange-500" role="status">强停提交结果尚未确认，请使用原强停请求核对；不会重放任务。</p>
        <div v-if="detail.data.value.force_stop" class="mb-3 text-sm" role="status">
          <p class="mb-1">任务编排已强制停止，历史记录保留。</p>
          <p v-if="detail.data.value.force_stop.reconciliation_completed" class="text-muted-foreground mb-0">外部结果核对已完成；资源与计费仍以实际记录为准。</p>
          <p v-else-if="detail.data.value.force_stop.requires_reconciliation" class="mb-0 text-orange-500">已发出的外部操作不保证停止，资源与计费不视为已释放。请请求核对，勿重复提交原操作。</p>
          <p v-else class="text-muted-foreground mb-0">服务端确认尚未派发外部操作；资源与计费以实际记录为准。</p>
        </div>
        <Descriptions bordered :column="1" size="small">
          <DescriptionsItem label="类型">{{ TASK_TYPES[detail.data.value.type] ?? detail.data.value.type }}</DescriptionsItem>
          <DescriptionsItem label="状态"><StatusTag kind="task" :value="detail.data.value.status" /></DescriptionsItem>
          <DescriptionsItem label="阶段"><span class="font-mono">{{ detail.data.value.phase || '—' }}</span></DescriptionsItem>
          <DescriptionsItem v-if="detail.data.value.type === 'image.prepare'" label="节点镜像进度">
            <span v-if="detail.data.value.node_id">节点 #{{ detail.data.value.node_id }}</span>
            <div v-if="typeof detail.data.value.result?.progress === 'string'" class="font-mono">{{ detail.data.value.result.progress }}</div>
          </DescriptionsItem>
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

    <TaskForceStopConfirm
      v-if="detail.data.value && selectedId"
      :key="selectedId"
      v-model:open="stopOpen"
      :task-id="selectedId"
      :label="TASK_TYPES[detail.data.value.type] ?? detail.data.value.type"
      :disabled="detail.loading.value || !canStop(detail.data.value) || (detail.data.value.can_force_stop === false && !stopState?.uncertain)"
      :pending="stopState?.pending"
      :uncertain="stopState?.uncertain"
      :feedback="stopState?.feedback"
      :on-confirm="forceStop"
    />
    <ConfirmAction
      v-model:open="reconcileOpen"
      description="重新查询节点上的实际状态，并根据结果结束该任务。不会重新执行原操作。"
      :on-confirm="reconcile"
      title="请求核对任务"
    />
  </Page>
</template>
