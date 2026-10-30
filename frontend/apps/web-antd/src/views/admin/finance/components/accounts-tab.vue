<script lang="ts" setup>
import type { CreditAccount, PolicyVersion } from '#/api/types';

import { onMounted, ref } from 'vue';

import { Input, Space, Table } from 'ant-design-vue';

import { financeApi } from '#/api/backend';
import CursorPager from '#/components/cursor-pager.vue';
import StatusTag from '#/components/status-tag.vue';
import TimeText from '#/components/time-text.vue';
import UserName from '#/components/user-name.vue';
import { useCursorList } from '#/composables/api';
import { formatPt } from '#/utils/format';

import AccountDrawer from './account-drawer.vue';
import { policyLabel } from './credit-policy';

const props = defineProps<{ userId?: string; versions: PolicyVersion[] }>();

const filters = ref<{ user_id: string | undefined }>({ user_id: props.userId });
const list = useCursorList<CreditAccount>(financeApi.accounts, filters);

const columns = [
  { dataIndex: 'id', title: 'ID', width: 80 },
  { key: 'user', title: '用户' },
  { key: 'status', title: '状态', width: 90 },
  { key: 'available', title: '可用', align: 'right' as const },
  { key: 'reserved', title: '已预留', align: 'right' as const },
  { key: 'policy', title: '额度政策' },
  { key: 'enforcement', title: '额度事件' },
];

const selectedId = ref<null | string>(null);

onMounted(() => list.load());
</script>

<template>
  <div>
    <Space class="mb-3" wrap>
      <Input v-model:value="filters.user_id" allow-clear class="w-36" placeholder="用户 ID" />
    </Space>
    <Table
      :columns="columns"
      :custom-row="(r: CreditAccount) => ({ onClick: () => (selectedId = r.id), style: 'cursor:pointer' })"
      :data-source="list.items.value"
      :loading="list.loading.value"
      :pagination="false"
      row-key="id"
      size="small"
    >
      <template #bodyCell="{ column, record }">
        <template v-if="column.key === 'user'"><UserName :id="record.user_id" /></template>
        <template v-else-if="column.key === 'status'"><StatusTag kind="accountStatus" :value="record.status" /></template>
        <template v-else-if="column.key === 'available'">
          <span class="font-mono">{{ formatPt(record.available_pt) }}</span>
        </template>
        <template v-else-if="column.key === 'reserved'">
          <span class="font-mono">{{ formatPt(record.reserved_pt) }}</span>
        </template>
        <template v-else-if="column.key === 'policy'">
          <span class="font-mono">{{ policyLabel(versions, record.policy_version_id) }}</span>
        </template>
        <template v-else-if="column.key === 'enforcement'">
          <Space v-if="record.enforcement" :size="4">
            <StatusTag kind="enforcement" :value="record.enforcement.status" />
            <TimeText :value="record.enforcement.shutdown_deadline" />
          </Space>
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
    <AccountDrawer v-model:id="selectedId" :versions="versions" @changed="list.load" />
  </div>
</template>
