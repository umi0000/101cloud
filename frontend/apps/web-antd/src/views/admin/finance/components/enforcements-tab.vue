<script lang="ts" setup>
import type { CreditEnforcement, PolicyVersion } from '#/api/types';

import { onMounted, ref } from 'vue';

import { Input, Select, Space, Table } from 'ant-design-vue';

import { financeApi } from '#/api/backend';
import CursorPager from '#/components/cursor-pager.vue';
import StatusTag from '#/components/status-tag.vue';
import TimeText from '#/components/time-text.vue';
import UserName from '#/components/user-name.vue';
import { useCursorList } from '#/composables/api';
import { ENUMS } from '#/locales/enums';

import EnforcementDrawer from './enforcement-drawer.vue';

const props = defineProps<{ userId?: string; versions: PolicyVersion[] }>();

const filters = ref<Record<'status' | 'user_id', string | undefined>>({ status: undefined, user_id: props.userId });
const list = useCursorList<CreditEnforcement>(financeApi.enforcements, filters);
const statusOptions = Object.entries(ENUMS.enforcement ?? {}).map(([value, l]) => ({ label: l.text, value }));

const columns = [
  { dataIndex: 'id', title: 'ID', width: 80 },
  { key: 'user', title: '用户' },
  { key: 'status', title: '状态', width: 100 },
  { key: 'exhausted', title: '额度耗尽', width: 130 },
  { key: 'deadline', title: '停机期限', width: 130 },
];

const selectedId = ref<null | string>(null);

onMounted(() => list.load());
</script>

<template>
  <div>
    <Space class="mb-3" wrap>
      <Select v-model:value="filters.status" allow-clear class="w-32" :options="statusOptions" placeholder="状态" />
      <Input v-model:value="filters.user_id" allow-clear class="w-36" placeholder="用户 ID" />
    </Space>
    <Table
      :columns="columns"
      :custom-row="(r: CreditEnforcement) => ({ onClick: () => (selectedId = r.id), style: 'cursor:pointer' })"
      :data-source="list.items.value"
      :loading="list.loading.value"
      :pagination="false"
      row-key="id"
      size="small"
    >
      <template #bodyCell="{ column, record }">
        <template v-if="column.key === 'user'"><UserName :id="record.user_id" /></template>
        <template v-else-if="column.key === 'status'"><StatusTag kind="enforcement" :value="record.status" /></template>
        <template v-else-if="column.key === 'exhausted'"><TimeText :value="record.exhausted_at" /></template>
        <template v-else-if="column.key === 'deadline'"><TimeText :value="record.shutdown_deadline" /></template>
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
    <EnforcementDrawer v-model:id="selectedId" :versions="versions" @changed="list.load" />
  </div>
</template>
