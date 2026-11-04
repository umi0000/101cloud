<script lang="ts" setup>
import type { CreationStageStatus, Instance, Task } from '#/api/types';
import type { ErrorText } from '#/locales/errors';

import { computed, onBeforeUnmount, ref, shallowRef, watch } from 'vue';
import { useRoute, useRouter } from 'vue-router';

import { useAccessStore } from '@vben/stores';

import { Button, Modal } from 'ant-design-vue';

import { memberApi } from '#/api/backend';
import { ApiError } from '#/api/request';
import ErrorFeedback from '#/components/error-feedback.vue';
import OsIcon from '#/components/os-icon.vue';
import { usePolling } from '#/composables/api';
import { describeError, describeTaskError } from '#/locales/errors';
import { useAuthStore } from '#/store/auth';
import { CREATION_STAGES, canEnterCreatedInstance, creationState, elapsedLabel } from '#/utils/creation-progress';

const route = useRoute();
const router = useRouter();
const auth = useAuthStore();
const access = useAccessStore();
const identity = computed(() => !access.loginExpired && auth.me?.user.role === 'user' ? auth.me.user.id : null);
const taskId = computed(() => typeof route.params.taskId === 'string' ? route.params.taskId : '');
const task = shallowRef<Task | null>(null);
const instance = shallowRef<Instance | null>(null);
const error = shallowRef<ErrorText | null>(null);
const summaryError = shallowRef<ErrorText | null>(null);
const loading = ref(false);
const denied = ref(false);
const failureOpen = ref(false);
const now = ref(Date.now());
let generation = 0;
let disposed = false;
const clock = window.setInterval(() => { now.value = Date.now(); }, 1000);

const state = computed(() => creationState(task.value));
const failed = computed(() => state.value === 'failed' || state.value === 'unknown');
const instanceName = computed(() => task.value?.creation_cleanup?.instance_name ?? instance.value?.name);
const cleanupLabels = {
  pending: '等待清理', cleaning: '正在清理', unknown: '回收结果待管理员核对', completed: '已完成清理',
};
const billingLabels = { pending: '等待处理', refunded: '已完成冲正、清欠与预留释放' };
watch(failed, (value) => { if (!value) failureOpen.value = false; });
const active = computed(() => !error.value && !denied.value && state.value === 'active');
const canEnter = computed(() => !error.value && !summaryError.value && canEnterCreatedInstance(task.value, instance.value, identity.value));
const elapsed = computed(() => elapsedLabel(task.value?.started_at ?? task.value?.created_at, task.value?.finished_at ?? now.value));
const stages = computed(() => CREATION_STAGES.map((definition) => ({
  ...definition,
  progress: task.value?.creation_progress?.stages.find((stage) => stage.key === definition.key),
})).filter((stage) => stage.key !== 'gpu' || stage.progress?.status !== 'skipped'));
const statusLabels: Record<CreationStageStatus, string> = {
  pending: '等待', running: '进行中', succeeded: '已完成', skipped: '不适用 · 已跳过', failed: '失败', unknown: '失败',
};
const taskFeedback = computed(() => {
  if (!task.value || !['failed', 'unknown'].includes(state.value)) return [];
  const candidates = [
    describeTaskError(task.value),
    ...task.value.steps.filter((step) => ['failed', 'unknown'].includes(step.status)).map((step) => describeTaskError(step)),
  ];
  const seen = new Set<string>();
  return candidates.filter((feedback): feedback is ErrorText => {
    if (!feedback) return false;
    const key = JSON.stringify([feedback.code, feedback.stage, feedback.text, feedback.details]);
    if (seen.has(key)) return false;
    seen.add(key);
    return true;
  });
});
const heading = computed(() => {
  if (!identity.value) return '会话不可用';
  if (denied.value) return '无法访问此初始化任务';
  if (error.value) return '状态读取中断';
  return {
    loading: '正在读取初始化状态', active: '正在初始化实例', succeeded: '实例初始化完成',
    failed: '初始化失败', unknown: '初始化失败', cancelled: '创建任务已取消',
    pending: '初始化尚未完成', unavailable: '初始化状态不可用',
  }[state.value];
});
const notice = computed(() => {
  if (!identity.value) return '请登录后重新打开此页面。';
  if (denied.value) return '任务不存在、已不可用，或当前账号无权访问。';
  if (error.value) return task.value ? '保留最后一次已确认的阶段；连接恢复前不能确认当前结果。' : '尚未读取到任务状态，请重试。';
  switch (state.value) {
    case 'pending': return instance.value?.desired_state === 'stopped' || instance.value?.observed_state === 'stopped'
      ? '实例已停止，初始化仍待完成；停止不代表初始化成功。' : '创建任务已结束，但初始化仍待完成。';
    case 'unknown': return '结果尚未确认，请联系管理员核对；不要重复创建。';
    case 'failed': return '任务已失败，不会自动重新创建。';
    case 'cancelled': return '任务已取消，未确认初始化完成。';
    case 'unavailable': return '服务端未提供完整的初始化阶段，不能确认成功。';
    default: return '';
  }
});

async function load() {
  if (disposed || loading.value || !identity.value || !taskId.value || denied.value) return;
  const token = generation;
  const userId = identity.value;
  const id = taskId.value;
  const current = () => !disposed && token === generation && identity.value === userId && taskId.value === id;
  loading.value = true;
  try {
    const response = await memberApi.task(id);
    if (!current()) return;
    if (response.data.id !== id || response.data.user_id !== userId || response.data.type !== 'instance.create') {
      denied.value = true;
      task.value = null;
      instance.value = null;
      return;
    }
    task.value = response.data;
    error.value = null;
    // 失败实例已从成员 API 隐藏；只使用任务提供的安全名称，不再读取配置摘要。
    if (response.data.creation_cleanup || !response.data.instance_id) {
      instance.value = null;
      summaryError.value = null;
      return;
    }
    try {
      const summary = await memberApi.instance(response.data.instance_id);
      if (!current()) return;
      if (summary.data.id !== response.data.instance_id || summary.data.user_id !== userId) {
        instance.value = null;
        summaryError.value = describeError(new ApiError(403, { code: 'forbidden', message: '无权访问此实例', details: null }, '', null));
        return;
      }
      instance.value = summary.data;
      summaryError.value = null;
    } catch (failure) {
      if (current()) {
        instance.value = null;
        summaryError.value = describeError(failure);
      }
    }
  } catch (failure) {
    if (!current()) return;
    if (failure instanceof ApiError && [401, 403, 404].includes(failure.status)) {
      denied.value = true;
      task.value = null;
      instance.value = null;
    }
    error.value = describeError(failure);
  } finally {
    if (current()) loading.value = false;
  }
}

watch([identity, taskId], () => {
  ++generation;
  failureOpen.value = false;
  task.value = null;
  instance.value = null;
  error.value = null;
  summaryError.value = null;
  denied.value = false;
  loading.value = false;
  void load();
}, { immediate: true, flush: 'sync' });
usePolling(() => {
  const cleanup = task.value?.creation_cleanup;
  if ((cleanup && (cleanup.status !== 'completed' || cleanup.billing_status !== 'refunded'))
    || state.value === 'active' || state.value === 'unknown' || error.value || summaryError.value || state.value === 'pending') return load();
}, () => 3000);
onBeforeUnmount(() => {
  disposed = true;
  ++generation;
  window.clearInterval(clock);
});

function enterInstance() {
  if (canEnter.value && task.value?.instance_id) void router.push({ path: '/instances', query: { instance: task.value.instance_id } });
}
</script>

<template>
  <main class="initialization-page">
    <section class="initialization-window" aria-labelledby="initialization-heading" :aria-busy="loading">
      <header class="window-bar">
        <div class="window-dots" aria-hidden="true"><i></i><i></i><i></i></div>
        <span>实例初始化</span>
        <span class="window-bar-spacer" aria-hidden="true"></span>
      </header>
      <div class="window-content">
        <div class="instance-heading">
          <OsIcon :name="instance?.image_os_name ?? ''" class="shrink-0 text-3xl" aria-hidden="true" />
          <div class="min-w-0 flex-1">
            <h1 id="initialization-heading">{{ heading }}</h1>
            <p class="instance-name">{{ instanceName ?? '实例信息待确认' }}</p>
          </div>
          <div class="elapsed"><span>耗时</span><strong>{{ elapsed }}</strong></div>
        </div>
        <p class="task-id">任务 <span>{{ taskId }}</span></p>
        <div v-if="summaryError" class="status-notice" role="status"><p>实例摘要读取失败</p><ErrorFeedback :feedback="summaryError" /></div>
        <div v-if="notice && !failed" class="status-notice" :class="{ 'is-error': error }" role="status"><p>{{ notice }}</p></div>
        <ErrorFeedback v-if="error && !denied" class="mb-4" :feedback="error" />
        <ol v-if="task?.creation_progress" class="stage-list" aria-label="初始化阶段">
          <li v-for="stage in stages" :key="stage.key" :data-stage="stage.key" :data-status="stage.progress?.status ?? 'unavailable'" :class="{ 'is-current': active && stage.progress?.status === 'running' }">
            <span class="stage-marker" aria-hidden="true">
              <span v-if="stage.progress?.status === 'succeeded'" class="icon-[lucide--check] size-3.5"></span>
              <span v-else-if="stage.progress?.status === 'failed'" class="icon-[lucide--x] size-3.5"></span>
              <span v-else-if="stage.progress?.status === 'skipped'" class="icon-[lucide--minus] size-3.5"></span>
              <span v-else-if="stage.progress?.status === 'unknown'" class="icon-[lucide--x] size-3.5"></span>
              <span v-else class="stage-dot"></span>
            </span>
            <span class="stage-label">{{ stage.label }}</span>
            <span class="stage-status">{{ stage.progress ? statusLabels[stage.progress.status] : '未提供' }}</span>
            <span class="stage-time">{{ stage.progress?.started_at ? elapsedLabel(stage.progress.started_at, stage.progress.finished_at ?? task.finished_at ?? now) : '—' }}</span>
          </li>
        </ol>
        <footer class="window-footer">
          <Button @click="router.push('/instances')">返回实例列表</Button>
          <Button v-if="failed" type="primary" @click="failureOpen = true">查看失败信息</Button>
          <Button v-if="error || summaryError || denied || state === 'unknown' || state === 'unavailable' || state === 'pending'" :disabled="!identity" :loading="loading" @click="denied = false; load()">重新查询</Button>
          <Button v-if="state === 'succeeded'" type="primary" :disabled="!canEnter" @click="enterInstance">进入实例</Button>
        </footer>
      </div>
      <div class="activity-line" :class="{ 'is-active': active }" aria-hidden="true"><span></span></div>
    </section>
    <Modal v-model:open="failureOpen" title="初始化失败信息" :footer="null" :width="560" destroy-on-close>
      <template v-if="failed">
        <p class="mb-4 text-sm">任务 {{ taskId }}<template v-if="instanceName"> · {{ instanceName }}</template></p>
        <p class="mb-4 text-sm">{{ state === 'unknown' ? '执行结果仍需管理员核对；不会自动重试，请勿重复创建或执行初始化。' : '任务已失败，不会自动重新创建。' }}</p>
        <div v-if="task?.creation_cleanup" class="mb-4 space-y-2 text-sm">
          <p>资源清理：{{ cleanupLabels[task.creation_cleanup.status] }}</p>
          <p>退款状态：{{ billingLabels[task.creation_cleanup.billing_status] }}</p>
          <p>未成功交付不收费，清理等待期间也不计费。</p>
        </div>
        <ErrorFeedback v-for="(feedback, index) in taskFeedback" :key="index" class="mb-4" :feedback="feedback" />
        <p v-if="!taskFeedback.length" class="mb-4 text-sm">未记录具体失败原因，请联系管理员检查原任务。</p>
      </template>
      <div class="flex justify-end"><Button @click="failureOpen = false">关闭</Button></div>
    </Modal>
  </main>
</template>

<style scoped>
.initialization-page { display: flex; min-height: calc(100dvh - 130px); align-items: center; justify-content: center; padding: 2rem 1rem; }
.initialization-window { width: min(720px, 100%); overflow: hidden; border: 1px solid hsl(var(--border)); border-radius: 14px; background: hsl(var(--card)); box-shadow: 0 16px 50px hsl(var(--foreground) / 0.06); }
.window-bar { display: flex; height: 44px; align-items: center; justify-content: space-between; border-bottom: 1px solid hsl(var(--border)); padding: 0 18px; color: hsl(var(--muted-foreground)); font-size: 12px; }
.window-dots { display: flex; width: 48px; gap: 6px; }
.window-dots i { width: 9px; height: 9px; border-radius: 50%; background: hsl(var(--muted-foreground) / 0.25); }
.window-dots i:nth-child(2) { opacity: 0.72; }
.window-dots i:nth-child(3) { opacity: 0.45; }
.window-bar-spacer { width: 48px; }
.window-content { padding: 28px 32px 24px; }
.instance-heading { display: flex; align-items: center; gap: 14px; }
h1 { margin: 0; font-size: 20px; font-weight: 600; line-height: 1.5; }
.instance-name { margin: 4px 0 0; overflow-wrap: anywhere; color: hsl(var(--muted-foreground)); font-size: 13px; }
.elapsed { display: flex; flex-shrink: 0; flex-direction: column; align-items: flex-end; gap: 4px; font-size: 12px; color: hsl(var(--muted-foreground)); }
.elapsed strong { color: hsl(var(--foreground)); font-size: 13px; font-weight: 500; font-variant-numeric: tabular-nums; }
.task-id { margin: 18px 0; overflow-wrap: anywhere; color: hsl(var(--muted-foreground)); font-size: 11px; }
.task-id span { margin-left: 8px; font-family: monospace; }
.stage-list { margin: 0; padding: 0; list-style: none; }
.stage-list li { position: relative; display: flex; min-height: 43px; align-items: center; gap: 12px; border-radius: 6px; padding: 0 10px; font-size: 13px; }
.stage-marker { position: relative; z-index: 1; display: flex; width: 22px; height: 22px; flex-shrink: 0; align-items: center; justify-content: center; border: 1px solid hsl(var(--border)); border-radius: 50%; background: hsl(var(--card)); color: hsl(var(--muted-foreground)); }
.stage-list li:not(:last-child)::after { position: absolute; top: 32px; bottom: -11px; left: 20px; width: 1px; background: hsl(var(--border)); content: ''; }
.stage-dot { width: 5px; height: 5px; border-radius: 50%; background: currentcolor; }
.stage-label { min-width: 0; flex: 1; }
.stage-status { color: hsl(var(--muted-foreground)); font-size: 11px; }
.stage-time { width: 70px; text-align: right; color: hsl(var(--muted-foreground)); font-size: 11px; font-variant-numeric: tabular-nums; }
.stage-list .is-current { color: hsl(var(--primary)); background: hsl(var(--primary) / 0.06); }
.is-current .stage-marker { border-color: hsl(var(--primary)); color: hsl(var(--primary)); }
.is-current .stage-status { color: hsl(var(--primary)); }
.stage-list [data-status='failed'] .stage-marker, .stage-list [data-status='failed'] .stage-status,
.stage-list [data-status='unknown'] .stage-marker, .stage-list [data-status='unknown'] .stage-status { color: hsl(var(--destructive)); }
.status-notice { margin: 0 0 16px; border-left: 2px solid hsl(var(--border)); padding: 2px 0 2px 12px; font-size: 12px; color: hsl(var(--muted-foreground)); }
.status-notice p { margin: 0; }
.status-notice.is-error { border-color: hsl(var(--destructive)); }
.window-footer { display: flex; justify-content: flex-end; flex-wrap: wrap; gap: 10px; margin-top: 26px; border-top: 1px solid hsl(var(--border)); padding-top: 20px; }
.window-footer > :first-child { margin-right: auto; }
.activity-line { height: 2px; overflow: hidden; background: hsl(var(--border) / 0.4); }
.activity-line span { display: none; width: 24%; height: 100%; background: hsl(var(--primary)); }
.activity-line.is-active span { display: block; animation: activity 2.4s ease-in-out infinite; }
@keyframes activity { from { transform: translateX(-100%); } to { transform: translateX(420%); } }
@media (prefers-reduced-motion: reduce) { .activity-line.is-active span { animation: none; transform: translateX(160%); } }
@media (max-width: 520px) { .initialization-page { align-items: flex-start; padding: 1rem 0.5rem; } .window-content { padding: 22px 16px 18px; } h1 { font-size: 18px; } .instance-heading { gap: 10px; } .stage-list li { gap: 8px; padding-right: 4px; } .stage-time { width: 55px; } .stage-status { max-width: 74px; text-align: right; } }
</style>
