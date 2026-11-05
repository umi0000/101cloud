<script lang="ts" setup>
import type { Task, TaskStatus, TaskStep } from '#/api/types';
import type { ErrorText } from '#/locales/errors';

import { computed, onBeforeUnmount, ref, shallowRef, watch } from 'vue';
import { RouterLink } from 'vue-router';
import { useAccessStore } from '@vben/stores';
import { Alert, Button, Card, Empty, Select, Spin } from 'ant-design-vue';

import { memberApi } from '#/api/backend';
import { ApiError } from '#/api/request';
import CursorPager from '#/components/cursor-pager.vue';
import ErrorFeedback from '#/components/error-feedback.vue';
import StatusTag from '#/components/status-tag.vue';
import TimeText from '#/components/time-text.vue';
import { usePolling } from '#/composables/api';
import { enumLabel, TASK_TYPES } from '#/locales/enums';
import { describeError, describeTaskError } from '#/locales/errors';
import { useAuthStore } from '#/store/auth';
import { elapsedLabel } from '#/utils/creation-progress';

const props = defineProps<{ instanceId: string }>();
const auth = useAuthStore();
const access = useAccessStore();
const identity = computed(() => !access.loginExpired && auth.me?.user.role === 'user' ? auth.me.user.id : null);
const STATUSES: TaskStatus[] = ['queued', 'running', 'waiting', 'succeeded', 'failed', 'unknown', 'cancelled'];
const statusOptions = [{ label: '全部状态', value: '' }, ...STATUSES.map((value) => ({ label: enumLabel('task', value).text, value }))];
const status = ref<TaskStatus | ''>('');
const items = shallowRef<Task[]>([]);
const loading = ref(false);
const error = shallowRef<ErrorText | null>(null);
const requestId = ref('');
const cursors = ref<(string | undefined)[]>([undefined]);
const nextCursor = ref<null | string>(null);
const pageNotice = ref('');
const expanded = ref<string[]>([]);
type DetailState = { data: null | Task; error: ErrorText | null; loading: boolean; requestId: string };
const details = shallowRef<Record<string, DetailState>>({});
const now = ref(Date.now());
const page = computed(() => cursors.value.length);
let generation = 0;
let listSeq = 0;
let viewGeneration = 0;
let disposed = false;
const detailSeq = new Map<string, number>();

function validTask(task: Task, userId: string, instanceId: string) {
  return task.user_id === userId && task.instance_id === instanceId;
}
function taskType(task: Task) {
  return task.type === 'instance.gpu' ? 'GPU 变更' : TASK_TYPES[task.type] ?? task.type;
}
function duration(task: Pick<Task | TaskStep, 'finished_at' | 'started_at' | 'status'>) {
  const end = task.finished_at ?? (['queued', 'running', 'waiting'].includes(task.status) ? now.value : '');
  return elapsedLabel(task.started_at, end);
}
function clearPage() {
  ++viewGeneration;
  ++listSeq;
  items.value = [];
  nextCursor.value = null;
  error.value = null;
  requestId.value = '';
  loading.value = false;
  expanded.value = [];
  details.value = {};
  detailSeq.clear();
}

async function load() {
  if (disposed || loading.value || !identity.value || !props.instanceId) return;
  const token = generation;
  const mine = ++listSeq;
  const userId = identity.value;
  const id = props.instanceId;
  const filter = status.value;
  const cursor = cursors.value.at(-1);
  const current = () => !disposed && token === generation && mine === listSeq
    && userId === identity.value && id === props.instanceId && filter === status.value && cursor === cursors.value.at(-1);
  loading.value = true;
  try {
    const response = await memberApi.tasks({ instance_id: id, status: filter || undefined, cursor, limit: 20 });
    if (!current()) return;
    if (response.data.items.some((task) => !validTask(task, userId, id))) {
      throw new ApiError(403, { code: 'forbidden', details: null, message: '无权查看此实例任务' }, response.requestId, null);
    }
    items.value = response.data.items;
    nextCursor.value = response.data.next_cursor;
    requestId.value = response.requestId;
    error.value = null;
    const ids = new Set(items.value.map((task) => task.id));
    expanded.value = expanded.value.filter((taskId) => ids.has(taskId));
    details.value = Object.fromEntries(Object.entries(details.value).filter(([taskId]) => ids.has(taskId)));
  } catch (failure) {
    if (!current()) return;
    if (failure instanceof ApiError && failure.code === 'invalid_cursor' && cursors.value.length > 1) {
      cursors.value = [undefined];
      pageNotice.value = '分页已失效，已回到第一页。';
      clearPage();
      await load();
      return;
    }
    if (failure instanceof ApiError && [401, 403, 404].includes(failure.status)) {
      items.value = [];
      nextCursor.value = null;
      expanded.value = [];
      details.value = {};
    }
    error.value = describeError(failure);
  } finally {
    if (current()) loading.value = false;
  }
}

async function loadDetail(taskId: string) {
  if (disposed || !identity.value || !expanded.value.includes(taskId) || details.value[taskId]?.loading) return;
  const token = generation;
  const view = viewGeneration;
  const mine = (detailSeq.get(taskId) ?? 0) + 1;
  detailSeq.set(taskId, mine);
  const userId = identity.value;
  const id = props.instanceId;
  const current = () => !disposed && token === generation && view === viewGeneration && mine === detailSeq.get(taskId)
    && userId === identity.value && id === props.instanceId && expanded.value.includes(taskId);
  details.value = { ...details.value, [taskId]: { data: details.value[taskId]?.data ?? null, error: null, loading: true, requestId: '' } };
  try {
    const response = await memberApi.task(taskId);
    if (!current()) return;
    if (response.data.id !== taskId || !validTask(response.data, userId, id)) {
      throw new ApiError(403, { code: 'forbidden', details: null, message: '无权查看此任务' }, response.requestId, null);
    }
    details.value = { ...details.value, [taskId]: { data: response.data, error: null, loading: false, requestId: response.requestId } };
  } catch (failure) {
    if (!current()) return;
    details.value = { ...details.value, [taskId]: { data: null, error: describeError(failure), loading: false, requestId: '' } };
  } finally {
    // 实例、身份、展开页切换后，不把迟到的详情写回视图。
    if (!disposed && token === generation && view === viewGeneration && mine === detailSeq.get(taskId) && details.value[taskId]?.loading) {
      details.value = { ...details.value, [taskId]: { data: null, error: null, loading: false, requestId: '' } };
    }
  }
}
function toggle(taskId: string) {
  if (expanded.value.includes(taskId)) {
    expanded.value = expanded.value.filter((id) => id !== taskId);
    detailSeq.set(taskId, (detailSeq.get(taskId) ?? 0) + 1);
    const remaining = { ...details.value };
    delete remaining[taskId];
    details.value = remaining;
  } else {
    expanded.value = [...expanded.value, taskId];
    void loadDetail(taskId);
  }
}
async function refresh() {
  await load();
  await Promise.all(expanded.value.map((taskId) => loadDetail(taskId)));
}
function nextPage() {
  if (loading.value || !nextCursor.value) return;
  cursors.value = [...cursors.value, nextCursor.value];
  pageNotice.value = '';
  clearPage();
  void load();
}
function previousPage() {
  if (loading.value || cursors.value.length <= 1) return;
  cursors.value = cursors.value.slice(0, -1);
  pageNotice.value = '';
  clearPage();
  void load();
}
watch([identity, () => props.instanceId, status], () => {
  ++generation;
  cursors.value = [undefined];
  pageNotice.value = '';
  clearPage();
  void load();
}, { immediate: true, flush: 'sync' });
usePolling(async () => { now.value = Date.now(); await refresh(); }, () => 5000);
onBeforeUnmount(() => { disposed = true; ++generation; ++listSeq; detailSeq.clear(); });
</script>

<template>
  <Card class="task-panel min-w-0" size="small" title="实例任务">
    <Alert v-if="!identity" message="请使用普通用户账号登录后查看任务。" show-icon type="info" />
    <template v-else>
      <div class="mb-3 flex flex-wrap items-end justify-between gap-3">
        <label><span class="mb-1 block text-xs">任务状态</span><Select v-model:value="status" aria-label="筛选任务状态" class="w-40" :options="statusOptions" /></label>
        <span class="text-muted-foreground text-xs">来自持久任务记录，每 5 秒刷新；不会重新执行操作。</span>
      </div>
      <Alert v-if="pageNotice" class="mb-3" :message="pageNotice" show-icon type="info" />
      <ErrorFeedback v-if="error" class="mb-3" :feedback="error" />
      <p v-if="error && items.length" class="text-muted-foreground mb-2 text-xs">保留上一次成功获取的任务记录；当前状态可能已变化。</p>
      <Spin :spinning="loading">
        <Empty v-if="!loading && !error && items.length === 0" description="没有符合筛选条件的任务" />
        <div class="space-y-3">
          <article v-for="task in items" :key="task.id" class="task-card">
            <div class="flex min-w-0 flex-wrap items-center justify-between gap-2">
              <h3 class="text-sm font-medium">{{ taskType(task) }}</h3>
              <StatusTag kind="task" :value="task.status" />
            </div>
            <p class="text-muted-foreground mt-1 break-all font-mono text-xs">任务 ID：{{ task.id }}</p>
            <dl class="task-times mt-3 text-xs">
              <div><dt>创建时间</dt><dd><TimeText :value="task.created_at" /></dd></div>
              <div><dt>开始时间</dt><dd><TimeText :value="task.started_at" /></dd></div>
              <div><dt>结束时间</dt><dd><TimeText :value="task.finished_at" /></dd></div>
              <div><dt>执行耗时</dt><dd>{{ duration(task) }}</dd></div>
            </dl>
            <Alert v-if="task.status === 'unknown'" class="mt-3" message="结果待核对：外部操作结果尚未确认，请联系管理员核对实际状态；不会自动重放操作。" show-icon type="warning" />
            <ErrorFeedback class="mt-2" :feedback="describeTaskError(task, requestId)" />
            <div class="mt-3 flex flex-wrap items-center gap-3">
              <Button size="small" :aria-expanded="expanded.includes(task.id)" @click="toggle(task.id)">{{ expanded.includes(task.id) ? '收起步骤' : '查看步骤' }}</Button>
              <RouterLink v-if="task.type === 'instance.create'" class="text-primary text-sm" :to="{ name: 'MemberInstanceInitialization', params: { taskId: task.id } }">查看实例初始化</RouterLink>
            </div>
            <div v-if="expanded.includes(task.id)" class="task-detail mt-3">
              <Spin :spinning="details[task.id]?.loading ?? false">
                <ErrorFeedback v-if="details[task.id]?.error" :feedback="details[task.id]?.error ?? null" />
                <template v-else-if="details[task.id]?.data">
                  <div class="mb-3 flex flex-wrap items-center gap-2 text-xs"><span>详情状态</span><StatusTag kind="task" :value="details[task.id]?.data?.status" /><span>执行耗时：{{ duration(details[task.id]?.data ?? task) }}</span></div>
                  <Alert v-if="details[task.id]?.data?.status === 'unknown'" class="mb-3" message="结果待管理员核对，不提供重试或重新执行。" show-icon type="warning" />
                  <ErrorFeedback class="mb-2" :feedback="describeTaskError(details[task.id]?.data, details[task.id]?.requestId)" />
                  <p v-if="details[task.id]?.data?.steps.length === 0" class="text-muted-foreground text-sm">服务端尚未记录步骤；不根据任务状态推测步骤完成情况。</p>
                  <ol v-else class="space-y-3">
                    <li v-for="(step, index) in details[task.id]?.data?.steps" :key="`${index}:${step.name}`" class="step-card">
                      <div class="flex flex-wrap items-center justify-between gap-2"><h4 class="break-words text-sm">{{ index + 1 }}. {{ step.name }}</h4><StatusTag kind="task" :value="step.status" /></div>
                      <dl class="task-times mt-2 text-xs">
                        <div><dt>开始</dt><dd><TimeText :value="step.started_at" /></dd></div>
                        <div><dt>结束</dt><dd><TimeText :value="step.finished_at" /></dd></div>
                        <div><dt>耗时</dt><dd>{{ duration(step) }}</dd></div>
                      </dl>
                      <p v-if="step.status === 'unknown'" class="mt-2 text-xs">此步骤结果待核对，不代表执行失败或可以重放。</p>
                      <ErrorFeedback class="mt-2" :feedback="describeTaskError(step, details[task.id]?.requestId)" />
                    </li>
                  </ol>
                </template>
                <p v-else-if="!details[task.id]?.loading" class="text-muted-foreground text-sm">等待获取任务详情。</p>
              </Spin>
            </div>
          </article>
        </div>
      </Spin>
      <CursorPager :has-next="!!nextCursor" :has-prev="page > 1" :loading="loading" :page="page" @next="nextPage" @prev="previousPage" @refresh="refresh" />
    </template>
  </Card>
</template>

<style scoped>
.task-panel { overflow-wrap: anywhere; }
.task-card { min-width: 0; border: 1px solid hsl(var(--border)); border-radius: 8px; padding: 16px; }
.task-times { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 10px 16px; }
.task-times dt { margin-bottom: 3px; color: hsl(var(--muted-foreground)); }
.task-detail { border-top: 1px solid hsl(var(--border)); padding-top: 12px; }
.step-card { border-left: 2px solid hsl(var(--border)); padding: 4px 0 4px 12px; }
@media (min-width: 900px) { .task-times { grid-template-columns: repeat(4, minmax(0, 1fr)); } }
@media (max-width: 520px) { .task-panel :deep(.ant-card-body) { padding: 12px; } .task-card { padding: 12px; } .task-panel :deep(.ant-space) { flex-wrap: wrap; justify-content: flex-end; } }
</style>
