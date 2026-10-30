<script lang="ts" setup>
import type { Dayjs } from 'dayjs';

import type { PolicyVersion } from '#/api/types';

import { computed, ref, watch } from 'vue';

import { Button, DatePicker, Descriptions, DescriptionsItem, Drawer, message, Space, Table } from 'ant-design-vue';

import { financeApi } from '#/api/backend';
import ConfirmAction from '#/components/confirm-action.vue';
import StatusTag from '#/components/status-tag.vue';
import TimeText from '#/components/time-text.vue';
import UserName from '#/components/user-name.vue';
import { errorToast, trackAccepted, useAction, useResource } from '#/composables/api';
import { toShanghaiISO } from '#/utils/format';

import { policyLabel } from './credit-policy';

defineProps<{ versions: PolicyVersion[] }>();
const emit = defineEmits<{ changed: [] }>();
const eventId = defineModel<null | string>('id', { default: null });

const detail = useResource(() => (eventId.value ? financeApi.enforcement(eventId.value) : null));
const open = computed({
  get: () => !!eventId.value,
  set: (v) => {
    if (!v) eventId.value = null;
  },
});

async function load() {
  try {
    await detail.reload();
  } catch (error) {
    errorToast(error);
  }
}

watch(eventId, (id) => {
  detail.data.value = null;
  detail.etag.value = null;
  if (id) void load();
});

async function refresh() {
  await load();
  emit('changed');
}

/** 后端 instances[] 为 credit_enforcement_instances 行的原样输出。 */
const instanceRows = computed(() => detail.data.value?.instances ?? []);
const cell = (v: unknown) => (v === null || v === undefined || v === '' ? '—' : String(v));

/* 延长停机期限：仅 grace 状态，新期限需晚于当前期限且晚于现在 → 200。 */
const extendOpen = ref(false);
const extendDeadline = ref<Dayjs | undefined>();
const extendAction = useAction();
watch(extendDeadline, () => extendAction.resetKey());

function openExtend() {
  extendDeadline.value = undefined;
  extendOpen.value = true;
}

async function submitExtend() {
  const current = detail.data.value;
  const deadline = extendDeadline.value;
  if (!current || !deadline) {
    message.warning('请选择新的停机期限');
    return false;
  }
  if (!deadline.isAfter(Date.now()) || !deadline.isAfter(Date.parse(current.shutdown_deadline))) {
    message.warning('新期限必须晚于当前期限和现在');
    return false;
  }
  const id = eventId.value;
  if (!id) return false;
  const body = { shutdown_deadline: toShanghaiISO(deadline.toDate()) };
  const r = await extendAction.run(
    (key) => financeApi.extend(id, body, { ifMatch: detail.etag.value, key }),
    { onConflict: load, success: '已延长停机期限' },
  );
  if (!r) return false;
  await refresh();
  return true;
}

/* 核对：后端请求体只接受 {} → 202。 */
const reconcileOpen = ref(false);
const reconcileAction = useAction();

async function submitReconcile() {
  const id = eventId.value;
  if (!id) return false;
  const r = await reconcileAction.run(
    (key) => financeApi.reconcileEnforcement(id, { ifMatch: detail.etag.value, key }),
    { onConflict: load, success: '已请求核对' },
  );
  if (!r) return false;
  trackAccepted(r.data, '额度事件核对', () => {
    if (eventId.value === id) void load();
    emit('changed');
  });
  await load();
  return true;
}
</script>

<template>
  <Drawer v-model:open="open" :title="`额度事件 #${eventId ?? ''}`" :width="600">
    <template v-if="detail.data.value" #extra>
      <Space>
        <Button v-if="detail.data.value.status === 'grace'" @click="openExtend">延长期限</Button>
        <Button type="primary" @click="reconcileOpen = true">请求核对</Button>
      </Space>
    </template>
    <template v-if="detail.data.value">
      <Descriptions bordered :column="1" size="small">
        <DescriptionsItem label="用户"><UserName :id="detail.data.value.user_id" link /></DescriptionsItem>
        <DescriptionsItem label="状态"><StatusTag kind="enforcement" :value="detail.data.value.status" /></DescriptionsItem>
        <DescriptionsItem label="额度耗尽"><TimeText :value="detail.data.value.exhausted_at" /></DescriptionsItem>
        <DescriptionsItem label="停机期限"><TimeText :value="detail.data.value.shutdown_deadline" /></DescriptionsItem>
        <DescriptionsItem label="额度政策">
          <span class="font-mono">{{ policyLabel(versions, detail.data.value.policy_version_id) }}</span>
        </DescriptionsItem>
        <DescriptionsItem label="账户">
          <span class="font-mono">{{ detail.data.value.account_id ? `#${detail.data.value.account_id}` : '—' }}</span>
        </DescriptionsItem>
        <DescriptionsItem label="实例">
          <Space v-if="detail.data.value.instance_ids.length" wrap>
            <RouterLink v-for="iid in detail.data.value.instance_ids" :key="iid" :to="`/admin/instances/${iid}`">
              <span class="font-mono">#{{ iid }}</span>
            </RouterLink>
          </Space>
          <span v-else>—</span>
        </DescriptionsItem>
        <DescriptionsItem label="通知">
          <Space v-if="detail.data.value.notification" wrap>
            <span class="font-mono">#{{ detail.data.value.notification.id }}</span>
            <span>发送</span>
            <TimeText :value="detail.data.value.notification.created_at" />
            <span>已读</span>
            <TimeText :value="detail.data.value.notification.read_at" />
          </Space>
          <span v-else>—</span>
        </DescriptionsItem>
      </Descriptions>
      <div v-if="detail.data.value.status === 'unknown'" class="mt-3 text-sm text-orange-500">
        停机结果未确认。核对只重查额度与实例实际状态，不会伪造“已停止”。
      </div>

      <template v-if="instanceRows.length">
        <h4 class="mb-2 mt-4 font-medium">实例停机记录</h4>
        <Table
          :columns="[
            { key: 'instance', title: '实例' },
            { key: 'status', title: '状态' },
            { key: 'task', title: '任务' },
            { key: 'updated', title: '更新' },
          ]"
          :data-source="instanceRows"
          :pagination="false"
          :row-key="(r: Record<string, unknown>) => cell(r.id)"
          size="small"
        >
          <template #bodyCell="{ column, record }">
            <template v-if="column.key === 'instance'">
              <RouterLink v-if="record.instance_id" :to="`/admin/instances/${record.instance_id}`">
                <span class="font-mono">#{{ record.instance_id }}</span>
              </RouterLink>
              <span v-else>—</span>
            </template>
            <template v-else-if="column.key === 'status'"><span class="font-mono">{{ cell(record.status) }}</span></template>
            <template v-else-if="column.key === 'task'"><span class="font-mono">{{ cell(record.task_id) }}</span></template>
            <template v-else-if="column.key === 'updated'"><TimeText :value="record.updated_at" /></template>
          </template>
        </Table>
      </template>
    </template>

    <ConfirmAction
      v-model:open="extendOpen"
      description="仅宽限中的事件可延长；新期限必须晚于当前期限。用户会收到通知。"
      :on-confirm="submitExtend"
      title="延长停机期限"
    >
      <div class="mb-3">
        <div class="mb-1 text-sm">新的停机期限</div>
        <DatePicker v-model:value="extendDeadline" class="w-full" show-time />
      </div>
    </ConfirmAction>

    <ConfirmAction
      v-model:open="reconcileOpen"
      description="重查额度与实例实际状态并推进该事件，不会伪造“已停止”。"
      :on-confirm="submitReconcile"
      title="请求核对额度事件"
    />
  </Drawer>
</template>
