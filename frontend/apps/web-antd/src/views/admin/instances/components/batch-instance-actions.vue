<script lang="ts" setup>
import type { Instance } from '#/api/types';

import { computed, onBeforeUnmount, ref } from 'vue';

import { Alert, Button, Checkbox, Modal, Table, Tag } from 'ant-design-vue';

import { instancesApi } from '#/api/backend';
import { ApiError, newIdempotencyKey } from '#/api/request';
import ErrorFeedback from '#/components/error-feedback.vue';
import { trackAccepted } from '#/composables/api';
import { describeError } from '#/locales/errors';

const props = defineProps<{ instances: Instance[]; loading: boolean }>();
const emit = defineEmits<{ accepted: [id: string]; changed: [] }>();

type Action = 'delete' | 'start' | 'stop';
interface Entry {
  instance: Instance;
  key: string;
  reason: string;
  status: 'accepted' | 'failed' | 'pending' | 'skipped' | 'unknown';
  feedback: null | ReturnType<typeof describeError>;
  taskId?: string;
}

const names: Record<Action, string> = { delete: '删除', start: '启动', stop: '停止' };
const statusNames = { accepted: '已受理', failed: '提交失败', pending: '待提交', skipped: '跳过', unknown: '受理结果待核对' };
const statusColors = { accepted: 'processing', failed: 'error', pending: 'default', skipped: 'default', unknown: 'warning' };
const open = ref(false);
const kind = ref<Action>('start');
const entries = ref<Entry[]>([]);
const pending = ref(false);
const submitted = ref(false);
const risk = ref(false);
let mounted = true;
onBeforeUnmount(() => { mounted = false; });

function unavailable(instance: Instance, action: Action): string {
  if (instance.desired_state === 'deleted' || instance.provisioning_state === 'deleted' || instance.provisioning_state === 'deleting') return '实例已删除或正在删除';
  if (instance.active_task_id) return '已有操作进行中';
  if (instance.provisioning_state !== 'ready') return '实例尚未就绪，请到详情页处理';
  if (instance.state_stale || !['running', 'stopped'].includes(instance.observed_state)) return '运行状态待核对';
  if (action === 'start' && instance.observed_state !== 'stopped') return '实例已在运行';
  if (action === 'stop' && instance.observed_state !== 'running') return '实例已经停止';
  if (!instance.capabilities[action]) return '当前实例不允许此操作';
  return '';
}

function eligibleCount(action: Action) {
  return props.instances.filter((instance) => !unavailable(instance, action)).length;
}

const actionable = computed(() => entries.value.filter((entry) => !entry.reason));
const canSubmit = computed(() => !pending.value && !submitted.value && actionable.value.length > 0
  && (kind.value !== 'delete' || risk.value));
const columns = [
  { key: 'name', title: '实例' },
  { key: 'result', title: '提交结果' },
];
const description = computed(() => kind.value === 'delete'
  ? '实例及磁盘数据将被永久删除，无法恢复；运行中的实例将被强制关闭，可能丢失未保存的数据。历史计费和审计记录保留。异常或操作中的实例请到详情页处理，不会批量强制清理。'
  : kind.value === 'stop' ? '对可操作的实例执行正常停止，不强制关机。' : '启动时将重新检查资源、额度及准入条件。');

function show(action: Action) {
  if (pending.value || props.loading || !eligibleCount(action)) return;
  kind.value = action;
  entries.value = props.instances.map((instance) => {
    const reason = unavailable(instance, action);
    return { instance: { ...instance }, key: newIdempotencyKey(), reason, status: reason ? 'skipped' : 'pending', feedback: null };
  });
  risk.value = false;
  submitted.value = false;
  open.value = true;
}

async function submit() {
  if (!canSubmit.value) return;
  pending.value = true;
  submitted.value = true;
  try {
    for (const entry of actionable.value) {
      if (!mounted) break;
      const instance = entry.instance;
      const write = { ifMatch: `"rev-${instance.revision}"`, key: entry.key };
      try {
        const response = kind.value === 'delete'
          ? await instancesApi.delete(instance.id, {
            confirm_name: instance.name, delete_data: true,
            ...(instance.observed_state === 'running' ? { mode: 'force' as const, confirm_data_loss_risk: true } : {}),
          }, write)
          : await instancesApi.power(instance.id, kind.value, kind.value === 'stop' ? { mode: 'graceful' } : {}, write);
        if (!mounted) break;
        entry.status = 'accepted';
        entry.taskId = response.data.task_id;
        trackAccepted(response.data, `${names[kind.value]}实例：${instance.name}`, () => { if (mounted) emit('changed'); });
        emit('accepted', instance.id);
      } catch (error) {
        if (!mounted) break;
        entry.feedback = describeError(error);
        entry.status = !(error instanceof ApiError) || error.status === 0 || error.status >= 500 ? 'unknown' : 'failed';
        if (error instanceof ApiError && error.status === 401) {
          for (const remaining of actionable.value) {
            if (remaining.status === 'pending') { remaining.status = 'skipped'; remaining.reason = '登录已失效，未提交'; }
          }
          break;
        }
      }
    }
  } finally {
    pending.value = false;
    if (mounted) emit('changed');
  }
}
</script>

<template>
  <Button :disabled="loading || pending || !eligibleCount('start')" data-testid="batch-start" @click="show('start')">批量启动</Button>
  <Button :disabled="loading || pending || !eligibleCount('stop')" data-testid="batch-stop" @click="show('stop')">批量停止</Button>
  <Button danger :disabled="loading || pending || !eligibleCount('delete')" data-testid="batch-delete" @click="show('delete')">批量删除</Button>

  <Modal v-model:open="open" :closable="!pending" destroy-on-close :keyboard="!pending" :mask-closable="false" :title="`批量${names[kind]}实例`" :width="760">
    <Alert class="mb-3" :message="description" show-icon :type="kind === 'delete' ? 'warning' : 'info'" />
    <div class="mb-3 text-sm" aria-live="polite">
      <template v-if="!submitted">已选 {{ entries.length }} 个，可提交 {{ actionable.length }} 个，跳过 {{ entries.length - actionable.length }} 个。</template>
      <template v-else>
        {{ pending ? '正在逐项提交' : '提交结束' }}：已受理 {{ entries.filter((entry) => entry.status === 'accepted').length }} 个，
        提交失败 {{ entries.filter((entry) => entry.status === 'failed').length }} 个，
        待核对 {{ entries.filter((entry) => entry.status === 'unknown').length }} 个，
        跳过 {{ entries.filter((entry) => entry.status === 'skipped').length }} 个。
        已受理不代表执行完成，请在任务中心查看最终结果；结果待核对的操作不会自动重发。
      </template>
    </div>
    <Table :columns="columns" :data-source="entries" :pagination="false" :row-key="(entry: Entry) => entry.instance.id" :scroll="{ x: 'max-content', y: 360 }" size="small">
      <template #bodyCell="{ column, record }">
        <template v-if="column.key === 'name'">
          <div>{{ record.instance.name }}</div>
          <div class="text-muted-foreground text-xs">#{{ record.instance.id }}</div>
        </template>
        <template v-else-if="column.key === 'result'">
          <Tag :color="statusColors[record.status as Entry['status']]">{{ statusNames[record.status as Entry['status']] }}</Tag>
          <span v-if="record.reason">{{ record.reason }}</span>
          <RouterLink v-if="record.taskId" :to="`/admin/tasks?instance_id=${record.instance.id}`">查看任务 #{{ record.taskId }}</RouterLink>
          <ErrorFeedback v-if="record.feedback" class="mt-1" :feedback="record.feedback" />
        </template>
      </template>
    </Table>
    <Checkbox v-if="kind === 'delete' && !submitted" v-model:checked="risk" class="mt-3">我确认永久删除以上可操作实例及磁盘，并承担强制关闭可能造成的数据丢失</Checkbox>
    <template #footer>
      <Button :disabled="pending" @click="open = false">{{ submitted ? '关闭' : '取消' }}</Button>
      <Button v-if="!submitted || pending" :danger="kind === 'delete'" :disabled="!canSubmit" :loading="pending" type="primary" @click="submit">确认{{ names[kind] }}</Button>
    </template>
  </Modal>
</template>
