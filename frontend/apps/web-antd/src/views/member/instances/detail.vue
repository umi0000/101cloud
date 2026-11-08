<script lang="ts" setup>
import type { MemberDeleteBody } from '#/api/backend';
import type {
  GPUAllocation,
  GPUPolicy,
  Instance,
  InstanceNetwork,
} from '#/api/types';
import type { ErrorText } from '#/locales/errors';

import { computed, onBeforeUnmount, ref, shallowRef, watch } from 'vue';
import { onBeforeRouteLeave, useRoute, useRouter } from 'vue-router';
import { Page } from '@vben/common-ui';
import { useAccessStore } from '@vben/stores';
import {
  Alert,
  Button,
  Card,
  Dropdown,
  Empty,
  Input,
  Menu,
  MenuItem,
  Modal,
  Spin,
  TabPane,
  Tabs,
  TypographyText,
} from 'ant-design-vue';

import { memberApi, memberConsoleApi } from '#/api/backend';
import { ApiError, newIdempotencyKey } from '#/api/request';
import ConfirmAction from '#/components/confirm-action.vue';
import ErrorFeedback from '#/components/error-feedback.vue';
import ConsoleDrawer from '#/components/instances/console-drawer.vue';
import ResizeDialog from '#/components/instances/resize-dialog.vue';
import OsIcon from '#/components/os-icon.vue';
import StatusTag from '#/components/status-tag.vue';
import TimeText from '#/components/time-text.vue';
import { usePolling } from '#/composables/api';
import { enumLabel, instanceState, instanceTaskState } from '#/locales/enums';
import { describeError } from '#/locales/errors';
import { useAuthStore } from '#/store/auth';
import { useTaskStore } from '#/store/tasks';
import { formatBytes } from '#/utils/format';
import { gpuAttachmentState } from '#/utils/gpu-attachment';

import GpuConfigPanel from './components/gpu-config-panel.vue';
import MetricsPanel from './components/metrics-panel.vue';
import TaskPanel from './components/task-panel.vue';

const route = useRoute();
const router = useRouter();
const auth = useAuthStore();
const access = useAccessStore();
const tasks = useTaskStore();
const identity = computed(() =>
  auth.me?.user.role === 'user' && !access.loginExpired
    ? auth.me.user.id
    : null,
);
const id = computed(() =>
  typeof route.params.id === 'string' ? route.params.id : '',
);
const instance = shallowRef<Instance | null>(null);
const etag = ref<string | null>(null);
const policy = shallowRef<GPUPolicy | null>(null);
const networks = shallowRef<InstanceNetwork[]>([]);
const allocations = shallowRef<GPUAllocation[]>([]);
const error = shallowRef<ErrorText | null>(null);
const networkLoaded = ref(false);
const gpuLoaded = ref(false);
const networkError = shallowRef<ErrorText | null>(null);
const gpuError = shallowRef<ErrorText | null>(null);
const policyError = shallowRef<ErrorText | null>(null);
const loading = ref(false);
const missing = ref(false);
let generation = 0;
let disposed = false;
const lifecycleEpoch = ref(0);
const viewKey = computed(
  () => `${identity.value}:${id.value}:${lifecycleEpoch.value}`,
);
const tab = computed({
  get: () =>
    typeof route.query.tab === 'string' &&
    ['overview', 'monitoring', 'gpu', 'tasks'].includes(route.query.tab)
      ? route.query.tab
      : 'overview',
  set: (value: string) => {
    void router.replace({ query: { ...route.query, tab: value } });
  },
});
const consoleRef = ref<InstanceType<typeof ConsoleDrawer>>();
const resizeOpen = ref(false);
const renameOpen = ref(false);
const name = ref('');
const powerKind = ref<'restart' | 'start' | 'stop'>('stop');
const powerOpen = ref(false);
const deleteOpen = ref(false);
const forceDeleteOpen = ref(false);
type CleanupConfirmation = {
  id: string;
  owner: string;
  name: string;
  etag: string;
  generation: number;
};
const cleanupConfirmation = shallowRef<CleanupConfirmation | null>(null);
const deleteAccepted = ref(false);
const cleanupAuthorizationLost = ref(false);
const submitting = ref(false);
const actionError = shallowRef<ErrorText | null>(null);
type Intent = {
  kind: 'delete' | 'force-delete' | 'rename' | 'restart' | 'start' | 'stop';
  id: string;
  key: string;
  etag: string;
  name: string;
  owner: string;
  deleteBody?: MemberDeleteBody;
};
const intent = shallowRef<Intent | null>(null);
const uncertain = ref(false);
const gpuVisited = ref(false);
const gpuUnresolved = ref(false);
const resizeUnresolved = ref(false);
const gpuSubmitting = ref(false);
const resizeSubmitting = ref(false);
const localSubmitting = computed(
  () => submitting.value || gpuSubmitting.value || resizeSubmitting.value,
);
const forceDeleteAuthorized = computed(
  () =>
    instance.value?.capabilities.force_delete === true &&
    instance.value.user_id === identity.value &&
    instance.value.id === id.value,
);
const canForceDelete = computed(
  () => forceDeleteAuthorized.value && !!etag.value && !error.value,
);
const forceDeleteDisabled = computed(
  () => !canForceDelete.value || localSubmitting.value || !!intent.value,
);
watch(forceDeleteAuthorized, (allowed) => {
  if (!allowed) {
    if (intent.value?.kind === 'force-delete')
      cleanupAuthorizationLost.value = true;
    forceDeleteOpen.value = false;
    cleanupConfirmation.value = null;
  }
});
watch(
  tab,
  (value) => {
    if (value === 'gpu') gpuVisited.value = true;
  },
  { immediate: true },
);
onBeforeRouteLeave(() => {
  if (!intent.value && !gpuUnresolved.value && !resizeUnresolved.value)
    return true;
  if (resizeUnresolved.value) resizeOpen.value = true;
  tab.value = gpuUnresolved.value ? 'gpu' : tab.value;
  return false;
});
function beforeUnload(event: BeforeUnloadEvent) {
  if (!intent.value && !gpuUnresolved.value && !resizeUnresolved.value) return;
  event.preventDefault();
  event.returnValue = '';
}
window.addEventListener('beforeunload', beforeUnload);
const busy = computed(
  () =>
    !!instance.value?.active_task_id ||
    submitting.value ||
    forceDeleteOpen.value ||
    !!intent.value ||
    gpuUnresolved.value ||
    resizeUnresolved.value,
);
const taskState = computed(() =>
  instance.value ? instanceTaskState(instance.value) : undefined,
);
const ready = computed(
  () =>
    !!instance.value &&
    !instance.value.state_stale &&
    instance.value.provisioning_state === 'ready' &&
    !!etag.value &&
    !error.value,
);
const actions = computed(() => policy.value?.allowed_user_actions ?? []);
const currentAllocations = computed(() =>
  allocations.value.filter(
    (a) => !['released', 'cancelled', 'failed'].includes(a.status),
  ),
);
const canRestart = computed(
  () =>
    ready.value &&
    instance.value?.observed_state === 'running' &&
    actions.value.includes('restart'),
);
const powerNames = { restart: '重启', start: '启动', stop: '停止' };
const enforcement = computed(() => {
  const value = instance.value?.credit_enforcement;
  if (
    !value ||
    ['normal', 'exempt', 'completed', 'cancelled'].includes(value.status)
  )
    return '';
  if (value.status === 'grace')
    return '额度不足，实例处于停机宽限期。请及时补充额度；新增 GPU 和换卡暂不可用。';
  return value.status === 'stopping'
    ? '额度停机处理中，请等待任务确认。'
    : '额度停机状态待核对，请联系管理员。';
});

async function reload() {
  if (
    !identity.value ||
    !id.value ||
    loading.value ||
    disposed ||
    missing.value
  )
    return;
  const owner = identity.value;
  const instanceId = id.value;
  const token = generation;
  const current = () =>
    !disposed &&
    token === generation &&
    identity.value === owner &&
    id.value === instanceId;
  loading.value = true;
  try {
    const response = await memberApi.instance(instanceId);
    if (!current()) return;
    if (response.data.user_id !== owner || response.data.id !== instanceId) {
      missing.value = true;
      instance.value = null;
      return;
    }
    instance.value = response.data;
    etag.value = response.etag;
    error.value = null;
    if (
      deleteAccepted.value &&
      response.data.observed_state === 'deleted' &&
      response.data.provisioning_state === 'deleted' &&
      !response.data.active_task_id
    ) {
      await router.replace('/instances');
      return;
    }
    if (response.data.active_task_id)
      tasks.track(response.data.active_task_id, '实例操作');
    await Promise.all([
      memberApi
        .network(instanceId)
        .then((r) => {
          if (current()) {
            networks.value = r.data;
            networkLoaded.value = true;
            networkError.value = null;
          }
        })
        .catch((e) => {
          if (current()) networkError.value = describeError(e);
        }),
      memberApi
        .gpuAllocations(instanceId)
        .then((r) => {
          if (current()) {
            allocations.value = r.data;
            gpuLoaded.value = true;
            gpuError.value = null;
          }
        })
        .catch((e) => {
          if (current()) gpuError.value = describeError(e);
        }),
      memberApi
        .gpuPolicy(instanceId)
        .then((r) => {
          if (current()) {
            policy.value = r.data;
            policyError.value = null;
          }
        })
        .catch((e) => {
          if (current()) {
            policy.value = null;
            policyError.value = describeError(e);
          }
        }),
    ]);
  } catch (failure) {
    if (!current()) return;
    if (failure instanceof ApiError && [403, 404].includes(failure.status)) {
      missing.value = true;
      instance.value = null;
      policy.value = null;
      networks.value = [];
      allocations.value = [];
      if (failure.status === 404 && deleteAccepted.value)
        await router.replace('/instances');
    } else error.value = describeError(failure);
  } finally {
    if (current()) loading.value = false;
  }
}
watch(
  [identity, id],
  () => {
    ++generation;
    instance.value = null;
    etag.value = null;
    policy.value = null;
    networks.value = [];
    allocations.value = [];
    error.value = null;
    networkError.value = null;
    gpuError.value = null;
    policyError.value = null;
    loading.value = false;
    missing.value = false;
    networkLoaded.value = false;
    gpuLoaded.value = false;
    resizeOpen.value = false;
    renameOpen.value = false;
    powerOpen.value = false;
    deleteOpen.value = false;
    forceDeleteOpen.value = false;
    cleanupConfirmation.value = null;
    deleteAccepted.value = false;
    cleanupAuthorizationLost.value = false;
    intent.value = null;
    uncertain.value = false;
    submitting.value = false;
    actionError.value = null;
    gpuVisited.value = tab.value === 'gpu';
    gpuUnresolved.value = false;
    resizeUnresolved.value = false;
    gpuSubmitting.value = false;
    resizeSubmitting.value = false;
    void reload();
  },
  { immediate: true, flush: 'sync' },
);
usePolling(reload, () =>
  instance.value?.active_task_id ||
  instance.value?.provisioning_state !== 'ready'
    ? 5000
    : 30_000,
);
watch(
  () =>
    tasks.tracked
      .filter((t) => t.task?.instance_id === id.value)
      .map((t) => `${t.taskId}:${t.task?.status}`)
      .join('|'),
  () => void reload(),
);
onBeforeUnmount(() => {
  disposed = true;
  ++generation;
  window.removeEventListener('beforeunload', beforeUnload);
});

function allowed(kind: Intent['kind']) {
  const value = instance.value;
  if (kind === 'force-delete') return canForceDelete.value;
  if (!value || !ready.value || value.active_task_id) return false;
  if (kind === 'rename') return true;
  if (kind === 'restart') return canRestart.value;
  if (kind === 'delete')
    return value.capabilities.delete && value.observed_state === 'stopped';
  return value.capabilities[kind];
}
function openPower(kind: 'restart' | 'start' | 'stop') {
  if (busy.value || !allowed(kind)) return;
  powerKind.value = kind;
  powerOpen.value = true;
}
function openRename() {
  if (busy.value || !allowed('rename')) return;
  name.value = instance.value?.name ?? '';
  renameOpen.value = true;
}
function openForceDelete() {
  if (forceDeleteDisabled.value || !identity.value || !instance.value || !etag.value)
    return;
  cleanupConfirmation.value = {
    id: id.value,
    owner: identity.value,
    name: instance.value.name,
    etag: etag.value,
    generation,
  };
  powerOpen.value = false;
  renameOpen.value = false;
  deleteOpen.value = false;
  resizeOpen.value = false;
  forceDeleteOpen.value = true;
}
async function submit(kind: Intent['kind']) {
  if (localSubmitting.value || !identity.value) return false;
  if (forceDeleteOpen.value && kind !== 'force-delete') return false;
  let pending = intent.value;
  if (!pending) {
    if (!allowed(kind) || !etag.value || !instance.value) return false;
    if (
      kind === 'rename' &&
      (!name.value.trim() || [...name.value].length > 128)
    )
      return false;
    const cleanup = kind === 'force-delete' ? cleanupConfirmation.value : null;
    if (
      kind === 'force-delete' &&
      (!cleanup ||
        cleanup.generation !== generation ||
        cleanup.id !== id.value ||
        cleanup.owner !== identity.value ||
        cleanup.name !== instance.value.name)
    ) {
      forceDeleteOpen.value = false;
      cleanupConfirmation.value = null;
      return false;
    }
    pending = {
      kind,
      id: id.value,
      owner: identity.value,
      etag: cleanup?.etag ?? etag.value,
      key: newIdempotencyKey(),
      name: kind === 'rename' ? name.value : (cleanup?.name ?? instance.value.name),
      deleteBody:
        kind === 'force-delete'
          ? {
              cleanup_stuck: true,
              mode: 'force',
              confirm_data_loss_risk: true,
              confirm_name: cleanup!.name,
              delete_data: true,
            }
          : kind === 'delete'
            ? { confirm_name: instance.value.name, delete_data: true }
            : undefined,
    };
    intent.value = pending;
  }
  if (
    pending.kind !== kind ||
    pending.id !== id.value ||
    pending.owner !== identity.value ||
    (kind === 'force-delete' &&
      (!canForceDelete.value || cleanupAuthorizationLost.value))
  )
    return false;
  const token = generation;
  submitting.value = true;
  actionError.value = null;
  const w = { ifMatch: pending.etag, key: pending.key };
  try {
    if (pending.kind === 'rename')
      await memberApi.rename(pending.id, pending.name, w);
    else {
      const response =
        pending.kind === 'delete' || pending.kind === 'force-delete'
          ? await memberApi.delete(
              pending.id,
              pending.deleteBody!,
              w,
            )
          : await memberApi.power(
              pending.id,
              pending.kind,
              pending.kind === 'start' ? {} : { mode: 'graceful' },
              w,
            );
      if (
        disposed ||
        token !== generation ||
        pending.owner !== identity.value ||
        pending.id !== id.value ||
        instance.value?.user_id !== pending.owner
      )
        return false;
      if (pending.kind === 'delete' || pending.kind === 'force-delete')
        deleteAccepted.value = true;
      if (pending.kind === 'force-delete') {
        // Discard old frozen GPU/resize intents only after cleanup is accepted.
        ++lifecycleEpoch.value;
        gpuUnresolved.value = false;
        resizeUnresolved.value = false;
        resizeOpen.value = false;
        powerOpen.value = false;
        renameOpen.value = false;
        deleteOpen.value = false;
        forceDeleteOpen.value = false;
        cleanupConfirmation.value = null;
      }
      tasks.track(
        response.data.task_id,
        `${pending.kind === 'force-delete' ? '强制清理删除' : pending.kind === 'delete' ? '销毁' : powerNames[pending.kind]}实例`,
      );
      tab.value = 'tasks';
    }
    if (disposed || token !== generation) return false;
    intent.value = null;
    uncertain.value = false;
    await reload();
    return true;
  } catch (failure) {
    if (disposed || token !== generation) return false;
    actionError.value = describeError(failure);
    uncertain.value =
      !(failure instanceof ApiError) ||
      failure.status === 0 ||
      failure.status === 408 ||
      failure.status >= 500;
    if (!uncertain.value) {
      intent.value = null;
      if (kind === 'force-delete') {
        if (failure instanceof ApiError && failure.status === 412)
          etag.value = null;
        forceDeleteOpen.value = false;
        cleanupConfirmation.value = null;
      }
      await reload();
    }
    return false;
  } finally {
    if (!disposed && token === generation) submitting.value = false;
  }
}
</script>

<template>
  <Page title="实例详情">
    <div class="mb-3">
      <Button @click="router.push('/instances')">返回实例列表</Button>
    </div>
    <Empty v-if="missing" description="实例不存在或当前账号无权访问" />
    <Spin v-else-if="!instance && loading" aria-label="正在读取实例详情" />
    <template v-else-if="identity && instance">
      <Card class="mb-4" size="small">
        <div class="flex flex-wrap items-start justify-between gap-4">
          <div class="flex min-w-0 items-start gap-3">
            <OsIcon
              :name="instance.image_os_name"
              class="mt-1 shrink-0 text-3xl"
            />
            <div class="min-w-0">
              <div class="flex flex-wrap items-center gap-2">
                <h1 class="m-0 break-all text-xl font-semibold">
                  {{ instance.name }}
                </h1>
                <StatusTag :label="instanceState(instance)" /><StatusTag
                  v-if="taskState"
                  :label="taskState"
                />
              </div>
              <p class="text-muted-foreground mb-0 mt-2 break-words text-sm">
                {{ enumLabel('instanceType', instance.instance_type).text }} ·
                ID {{ instance.id }} ·
                {{ enumLabel('productType', instance.product_type).text }}
              </p>
            </div>
          </div>
          <div class="flex flex-wrap gap-2">
            <Button :loading="loading" @click="reload">刷新</Button>
            <Button
              v-if="instance.capabilities.console"
              :disabled="busy || !ready"
              @click="consoleRef?.start()"
              >{{
                instance.instance_type === 'container' ? '终端' : '控制台'
              }}</Button
            >
            <Button
              v-if="instance.capabilities.start"
              type="primary"
              :disabled="busy || !ready"
              @click="openPower('start')"
              >启动</Button
            >
            <Button
              v-if="instance.capabilities.stop"
              :disabled="busy || !ready"
              @click="openPower('stop')"
              >停止</Button
            >
            <Dropdown>
              <Button>更多</Button>
              <template #overlay
                ><Menu>
                  <MenuItem :disabled="busy || !ready" @click="openRename"
                    >重命名</MenuItem
                  >
                  <MenuItem
                    v-if="actions.includes('restart')"
                    :disabled="busy || !canRestart"
                    @click="openPower('restart')"
                    >重启</MenuItem
                  >
                  <MenuItem
                    v-if="instance.capabilities.resize"
                    :disabled="busy || !ready"
                    @click="resizeOpen = true"
                    >修改实例规格</MenuItem
                  >
                  <MenuItem
                    v-if="actions.includes('delete')"
                    danger
                    :disabled="
                      busy ||
                      !ready ||
                      !instance.capabilities.delete ||
                      instance.observed_state !== 'stopped'
                    "
                    @click="deleteOpen = true"
                    >销毁实例（需先停止）</MenuItem
                  >
                </Menu></template
              >
            </Dropdown>
            <Button
              v-if="instance.capabilities.force_delete === true"
              danger
              :disabled="forceDeleteDisabled"
              @click="openForceDelete"
              >强制清理删除</Button
            >
          </div>
        </div>
      </Card>
      <Alert
        v-if="instance.state_stale"
        class="mb-3"
        type="warning"
        show-icon
        message="状态信息已过期，暂不能确认实例当前状态。普通变更暂停；有删除权限时仍可明确确认强制清理删除。"
      />
      <Alert
        v-if="enforcement"
        class="mb-3"
        type="warning"
        show-icon
        :message="enforcement"
      />
      <Alert
        v-if="taskState"
        class="mb-3"
        :type="taskState.type"
        show-icon
        :message="taskState.message"
        ><template #action
          ><Button size="small" @click="tab = 'tasks'"
            >查看任务</Button
          ></template
        ></Alert
      >
      <Alert
        v-if="deleteAccepted"
        class="mb-3"
        type="info"
        show-icon
        message="删除请求已受理，不表示实例或磁盘已删除。请在任务中心等待确认；结果待核对或失败时不会自动重放旧操作。"
      />
      <div v-if="error" class="mb-3" role="alert">
        <p>实例刷新失败，以下保留上次结果，变更操作暂停。</p>
        <ErrorFeedback :feedback="error" />
      </div>
      <div v-if="actionError" class="mb-3" role="alert">
        <ErrorFeedback :feedback="actionError" />
      </div>
      <Alert
        v-if="uncertain && intent"
        class="mb-3"
        type="warning"
        show-icon
        :message="
          cleanupAuthorizationLost
            ? '提交结果尚未确认，但删除权限或实例归属已变化，原请求禁止重试。请查看任务并联系管理员核对，不会自动替换为新操作。'
            : '提交结果尚未确认，配置已锁定。请勿重复发起新操作；可用原请求和原幂等键查询式重试。'
        "
        ><template #action
          ><Button
            :loading="submitting"
            :disabled="intent.kind === 'force-delete' && (!canForceDelete || cleanupAuthorizationLost)"
            @click="submit(intent.kind)"
            >重试原请求</Button
          ></template
        ></Alert
      >
      <Tabs v-model:active-key="tab">
        <TabPane key="overview" tab="概览">
          <div class="resource-grid mb-4">
            <Card size="small"
              ><p class="text-muted-foreground">CPU 配置</p>
              <strong class="text-2xl"
                >{{ instance.resources.vcpus }} vCPU</strong
              ></Card
            >
            <Card size="small"
              ><p class="text-muted-foreground">内存配置</p>
              <strong class="text-2xl">{{
                formatBytes(instance.resources.memory_bytes)
              }}</strong></Card
            >
            <Card size="small"
              ><p class="text-muted-foreground">磁盘配置容量</p>
              <strong class="text-2xl">{{
                formatBytes(instance.resources.disk_bytes)
              }}</strong></Card
            >
            <Card size="small"
              ><p class="text-muted-foreground">当前 GPU 分配</p>
              <strong class="text-2xl">{{
                !gpuLoaded
                  ? gpuError
                    ? '未知'
                    : '加载中'
                  : `${currentAllocations.length} 张`
              }}</strong></Card
            >
          </div>
          <div class="overview-grid">
            <Card title="基本信息" size="small">
              <dl class="detail-fields">
                <dt>来源镜像系统</dt>
                <dd>
                  {{ instance.image_os_name || '未知' }}
                  {{ instance.image_os_release }}
                </dd>
                <dt>实例类型</dt>
                <dd>
                  {{ enumLabel('instanceType', instance.instance_type).text }}
                </dd>
                <dt>套餐</dt>
                <dd>
                  {{
                    instance.plan_id
                      ? `套餐 #${instance.plan_id}`
                      : '自定义配置'
                  }}
                </dd>
                <dt>计费模式</dt>
                <dd>
                  {{
                    instance.billing_mode === 'admin_exempt'
                      ? '免额度'
                      : '额度计费'
                  }}
                </dd>
                <dt>创建时间</dt>
                <dd><TimeText :value="instance.created_at" /></dd>
                <dt>最近状态观测</dt>
                <dd><TimeText :value="instance.observed_at" /></dd>
              </dl>
              <p class="text-muted-foreground mb-0 mt-4 text-xs">
                来源镜像版本不代表实例内升级后的系统版本；磁盘配置容量不是已用空间。
              </p>
            </Card>
            <Card title="网络配置" size="small">
              <ErrorFeedback v-if="networkError" :feedback="networkError" />
              <div
                v-for="network in networks"
                :key="network.ip"
                class="mb-3 rounded border p-3"
              >
                <dl class="detail-fields">
                  <dt>IP 地址</dt>
                  <dd>
                    <TypographyText :copyable="{ text: network.ip }">{{
                      network.ip
                    }}</TypographyText>
                    / {{ network.prefix_length }}
                  </dd>
                  <dt>网关</dt>
                  <dd>{{ network.gateway || '—' }}</dd>
                  <dt>DNS</dt>
                  <dd>{{ network.dns.join('、') || '—' }}</dd>
                  <dt>模式 / 状态</dt>
                  <dd>
                    {{ enumLabel('networkMode', network.mode).text }} ·
                    {{
                      network.quality.configuration_bound
                        ? '已绑定'
                        : '尚未绑定'
                    }}<span v-if="network.quality.state_stale">
                      · 状态已过期</span
                    >
                  </dd>
                </dl>
              </div>
              <Empty
                v-if="networkLoaded && !networks.length && !networkError"
                description="暂无已分配网络"
              />
              <p
                v-else-if="!networkLoaded && !networkError"
                class="text-muted-foreground"
              >
                正在读取网络…
              </p>
            </Card>
            <Card title="GPU 分配摘要" size="small" class="overview-gpu">
              <ErrorFeedback v-if="gpuError" :feedback="gpuError" />
              <div
                v-for="allocation in currentAllocations"
                :key="allocation.id"
                class="mb-2 flex flex-wrap items-center gap-3"
              >
                <span>{{ allocation.model }}</span
                ><StatusTag
                  :label="
                    enumLabel('gpuAttachment', gpuAttachmentState(allocation))
                  "
                /><span
                  v-if="
                    ['unknown', 'releasing'].includes(allocation.status) &&
                    !allocation.release_verified
                  "
                  class="text-destructive"
                  >释放待确认</span
                >
              </div>
              <Empty
                v-if="gpuLoaded && !currentAllocations.length && !gpuError"
                description="当前未分配 GPU"
              />
              <p
                v-else-if="!gpuLoaded && !gpuError"
                class="text-muted-foreground"
              >
                正在读取 GPU 分配…
              </p>
              <Button
                v-if="instance.instance_type === 'container'"
                class="mt-2"
                @click="tab = 'gpu'"
                >管理 GPU 配置</Button
              >
            </Card>
          </div>
        </TabPane>
        <TabPane key="monitoring" tab="监控"
          ><MetricsPanel
            v-if="tab === 'monitoring'"
            :key="viewKey"
            :instance-id="instance.id"
        /></TabPane>
        <TabPane key="gpu" tab="GPU 配置" force-render
          ><GpuConfigPanel
            v-if="instance.instance_type === 'container' && gpuVisited"
            :key="viewKey"
            :instance="instance"
            :etag="etag"
            :active="tab === 'gpu'"
            :disabled="!!intent || forceDeleteOpen || resizeUnresolved || !!error"
            @unresolved="gpuUnresolved = $event"
            @submitting="gpuSubmitting = $event"
            @changed="reload" /><Empty
            v-else-if="instance.instance_type !== 'container'"
            description="当前 GPU 挂载与自动策略仅支持容器实例"
        /></TabPane>
        <TabPane key="tasks" tab="任务"
          ><TaskPanel
            v-if="tab === 'tasks'"
            :key="viewKey"
            :instance-id="instance.id"
        /></TabPane>
      </Tabs>
      <p v-if="policyError" class="text-muted-foreground text-xs">
        操作权限读取失败，重启和 GPU 策略等入口暂不可用。<ErrorFeedback
          :feedback="policyError"
        />
      </p>
      <ConsoleDrawer
        :key="`console:${viewKey}`"
        ref="consoleRef"
        :api="memberConsoleApi"
        :instance="instance"
        :etag="etag"
        :disabled="busy || !ready"
      />
      <ResizeDialog
        :key="`resize:${viewKey}`"
        v-model:open="resizeOpen"
        :api="memberApi"
        :instance="instance"
        :etag="etag"
        :disabled="gpuUnresolved || forceDeleteOpen || !!intent || !!error"
        @unresolved="resizeUnresolved = $event"
        @submitting="resizeSubmitting = $event"
        @done="reload"
      />
      <Modal
        v-model:open="renameOpen"
        title="重命名实例"
        :confirm-loading="submitting"
        :ok-button-props="{
          disabled: busy || !ready || !name.trim() || [...name].length > 128,
        }"
        @ok="
          async () => {
            if (await submit('rename')) renameOpen = false;
          }
        "
        ><Input
          v-model:value="name"
          :disabled="busy"
          :maxlength="128"
          placeholder="1–128 个字符"
      /></Modal>
      <ConfirmAction
        v-model:open="powerOpen"
        :title="`${powerNames[powerKind]}实例`"
        :disabled="busy || !allowed(powerKind)"
        :description="
          powerKind === 'start'
            ? '启动时重新检查额度和资源准入。'
            : '正常停止或重启会中断实例内的任务，请先保存工作。'
        "
        :on-confirm="() => submit(powerKind)"
      />
      <ConfirmAction
        v-model:open="deleteOpen"
        title="销毁实例"
        danger
        :confirm-name="instance.name"
        description="永久删除已停止实例及磁盘数据，无法恢复；历史账单和任务保留。"
        :disabled="busy || !allowed('delete')"
        :on-confirm="() => submit('delete')"
      />
      <ConfirmAction
        v-if="cleanupConfirmation"
        :key="`cleanup:${cleanupConfirmation.generation}:${cleanupConfirmation.etag}`"
        v-model:open="forceDeleteOpen"
        title="强制清理删除实例"
        ok-text="强制清理删除"
        danger
        :confirm-name="cleanupConfirmation.name"
        description="此操作将取消已有操作，强制关闭并永久删除实例及全部磁盘，未保存的数据会丢失且无法恢复。节点权威确认原生实例不存在时才跳过原生操作；离线或请求失败不代表已删除。历史计费、任务和审计记录保留。提交后请等待任务确认。"
        risk-text="我确认取消旧操作、强制关闭可能丢失未保存数据，并永久删除实例及全部磁盘，此操作不可恢复"
        :disabled="forceDeleteDisabled"
        :on-confirm="() => submit('force-delete')"
      />
    </template>
    <template v-else-if="error"
      ><ErrorFeedback :feedback="error" /><Button @click="reload"
        >重新加载</Button
      ></template
    >
  </Page>
</template>

<style scoped>
.resource-grid {
  display: grid;
  grid-template-columns: repeat(4, minmax(0, 1fr));
  gap: 1rem;
}
.overview-grid {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 1rem;
}
.overview-gpu {
  grid-column: 1 / -1;
}
.detail-fields {
  display: grid;
  grid-template-columns: minmax(6rem, 0.4fr) minmax(0, 1fr);
  gap: 0.85rem 1rem;
  margin: 0;
}
.detail-fields dt {
  color: hsl(var(--muted-foreground));
}
.detail-fields dd {
  margin: 0;
  overflow-wrap: anywhere;
}
@media (max-width: 900px) {
  .resource-grid {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }
  .overview-grid {
    grid-template-columns: minmax(0, 1fr);
  }
}
@media (max-width: 420px) {
  .resource-grid {
    gap: 0.5rem;
  }
  .resource-grid strong {
    font-size: 1.2rem;
  }
}
</style>
