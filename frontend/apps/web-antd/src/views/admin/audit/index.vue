<script lang="ts" setup>
import type { Dayjs } from 'dayjs';

import type { Query } from '#/api/backend';
import type { AuditLog } from '#/api/types';

import { onMounted, ref, watch } from 'vue';

import { Page } from '@vben/common-ui';

import { Card, DatePicker, Input, Table } from 'ant-design-vue';

import { opsApi } from '#/api/backend';
import CursorPager from '#/components/cursor-pager.vue';
import TimeText from '#/components/time-text.vue';
import UserName from '#/components/user-name.vue';
import { useCursorList } from '#/composables/api';
import { AUDIT_EVENTS } from '#/locales/enums';
import { toShanghaiISO } from '#/utils/format';

const filters = ref<Query>({});
const actor = ref('');
const instance = ref('');
const task = ref('');
const range = ref<[Dayjs, Dayjs] | undefined>();

watch([actor, instance, task, range], () => {
  filters.value = {
    actor_user_id: actor.value || undefined,
    from: range.value ? toShanghaiISO(range.value[0].toDate()) : undefined,
    instance_id: instance.value || undefined,
    task_id: task.value || undefined,
    to: range.value ? toShanghaiISO(range.value[1].toDate()) : undefined,
  };
});

const list = useCursorList<AuditLog>(opsApi.audit, filters);

const columns = [
  { key: 'time', title: '时间', width: 130 },
  { key: 'actor', title: '操作者' },
  { key: 'event', title: '事件' },
  { key: 'resource', title: '资源' },
  { dataIndex: 'result', title: '结果', width: 90 },
  { key: 'task', title: '任务', width: 90 },
  { dataIndex: 'request_id', title: 'request_id' },
];

onMounted(() => list.load());
</script>

<template>
  <Page description="只读检索。列表不返回操作详情。" title="审计日志">
    <Card size="small">
      <div class="mb-3 flex flex-wrap items-center gap-2">
        <Input v-model:value="actor" allow-clear class="!w-36" placeholder="操作者用户 ID" />
        <Input v-model:value="instance" allow-clear class="!w-32" placeholder="实例 ID" />
        <Input v-model:value="task" allow-clear class="!w-32" placeholder="任务 ID" />
        <DatePicker.RangePicker v-model:value="range" class="max-w-full" show-time />
      </div>
      <Table
        :columns="columns"
        :data-source="list.items.value"
        :loading="list.loading.value"
        :pagination="false"
        :scroll="{ x: 'max-content' }"
        row-key="id"
        size="small"
      >
        <template #bodyCell="{ column, record }">
          <template v-if="column.key === 'time'"><TimeText :value="record.occurred_at" /></template>
          <template v-else-if="column.key === 'actor'"><UserName :id="record.actor_user_id" link /></template>
          <template v-else-if="column.key === 'event'">
            {{ AUDIT_EVENTS[record.event_type] ?? record.event_type }}
            <span class="text-muted-foreground ml-1 font-mono text-xs">{{ record.event_type }}</span>
          </template>
          <template v-else-if="column.key === 'resource'">
            <RouterLink
              v-if="record.resource_type === 'instance' && record.resource_id"
              :to="`/admin/instances/${record.resource_id}`"
            >
              实例 #{{ record.resource_id }}
            </RouterLink>
            <span v-else class="font-mono text-xs">{{ record.resource_type }}{{ record.resource_id ? ` #${record.resource_id}` : '' }}</span>
          </template>
          <template v-else-if="column.key === 'task'">
            <RouterLink v-if="record.task_id" :to="{ path: '/admin/tasks' }">#{{ record.task_id }}</RouterLink>
            <span v-else>—</span>
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
  </Page>
</template>
