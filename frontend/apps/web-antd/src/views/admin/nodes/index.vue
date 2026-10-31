<script lang="ts" setup>
import type { Node } from '#/api/types';

import { onMounted, ref } from 'vue';
import { useRouter } from 'vue-router';

import { Page } from '@vben/common-ui';

import { Button, Card, Select, Space, Table, Tag } from 'ant-design-vue';

import { nodesApi } from '#/api/backend';
import CursorPager from '#/components/cursor-pager.vue';
import StatusTag from '#/components/status-tag.vue';
import TimeText from '#/components/time-text.vue';
import { useCursorList } from '#/composables/api';
import { ENUMS } from '#/locales/enums';
import { secondsSince } from '#/utils/format';

import RegisterDrawer from './components/register-drawer.vue';

type NodeFilters = Record<'administrative_state' | 'health', string | undefined>;
const filters = ref<NodeFilters>({ administrative_state: undefined, health: undefined });
const list = useCursorList<Node>(nodesApi.list, filters);

const options = (kind: string) => Object.entries(ENUMS[kind] ?? {}).map(([value, l]) => ({ label: l.text, value }));

const columns = [
  { key: 'name', title: '名称' },
  { key: 'state', title: '管理状态', width: 100 },
  { key: 'health', title: '健康', width: 80 },
  { key: 'endpoint', title: 'Agent 地址' },
  { key: 'heartbeat', title: '最近心跳', width: 120 },
  { key: 'sampled', title: '库存时间', width: 170 },
];

const router = useRouter();
const registerOpen = ref(false);

function inventoryStale(n: Node) {
  const age = secondsSince(n.resources_sampled_at);
  return age !== null && age > 60;
}

onMounted(() => list.load());
</script>

<template>
  <Page title="节点与 GPU">
    <Card size="small">
      <div class="mb-3 flex flex-wrap items-center justify-between gap-2">
        <Space wrap>
          <Select
            v-model:value="filters.administrative_state"
            allow-clear
            class="w-32"
            :options="options('adminState')"
            placeholder="管理状态"
          />
          <Select v-model:value="filters.health" allow-clear class="w-28" :options="options('health')" placeholder="健康" />
        </Space>
        <Button type="primary" @click="registerOpen = true">登记节点</Button>
      </div>
      <Table
        :columns="columns"
        :data-source="list.items.value"
        :loading="list.loading.value"
        :pagination="false"
        row-key="id"
        size="small"
      >
        <template #bodyCell="{ column, record }">
          <template v-if="column.key === 'name'">
            <RouterLink :to="`/admin/nodes/${record.id}`">{{ record.name }}</RouterLink>
            <span class="text-muted-foreground ml-1 font-mono text-xs">#{{ record.id }}</span>
          </template>
          <template v-else-if="column.key === 'state'">
            <StatusTag kind="adminState" :value="record.administrative_state" />
          </template>
          <template v-else-if="column.key === 'health'"><StatusTag kind="health" :value="record.health" /></template>
          <template v-else-if="column.key === 'endpoint'">
            <span class="font-mono text-xs">{{ record.connection.endpoint }}</span>
          </template>
          <template v-else-if="column.key === 'heartbeat'"><TimeText :value="record.last_heartbeat_at" /></template>
          <template v-else-if="column.key === 'sampled'">
            <TimeText :value="record.resources_sampled_at" />
            <Tag v-if="inventoryStale(record as Node)" :bordered="false" class="ml-1" color="warning">库存已过期</Tag>
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
    <RegisterDrawer v-model:open="registerOpen" @created="(n: Node) => router.push(`/admin/nodes/${n.id}`)" />
  </Page>
</template>
