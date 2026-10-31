<script lang="ts" setup>
import type { NetworkPoolView } from './network';

import { onMounted, ref, shallowRef } from 'vue';

import { Button, Space, Table } from 'ant-design-vue';

import { networkApi } from '#/api/backend';
import CursorPager from '#/components/cursor-pager.vue';
import StatusTag from '#/components/status-tag.vue';
import { useCursorList } from '#/composables/api';

import PoolAddresses from './pool-addresses.vue';
import PoolDrawer from './pool-drawer.vue';

const list = useCursorList<NetworkPoolView>(networkApi.pools, ref({}));
onMounted(() => list.load());

const columns = [
  { key: 'name', title: '名称' },
  { key: 'cidr', title: '网段' },
  { key: 'range', title: '分配范围' },
  { key: 'gateway', title: '网关' },
  { key: 'dns', title: 'DNS' },
  { key: 'status', title: '状态', width: 80 },
  { key: 'op', title: '', width: 120 },
];

const drawerOpen = ref(false);
const editing = shallowRef<NetworkPoolView | null>(null);
const addressesOpen = ref(false);
const addressesPool = shallowRef<NetworkPoolView | null>(null);

function openDrawer(pool: NetworkPoolView | null) {
  editing.value = pool;
  drawerOpen.value = true;
}
function openAddresses(pool: NetworkPoolView) {
  addressesPool.value = pool;
  addressesOpen.value = true;
}
</script>

<template>
  <div>
    <div class="mb-3 flex justify-end">
      <Button type="primary" @click="openDrawer(null)">登记地址池</Button>
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
          {{ record.name }}
          <span class="text-muted-foreground ml-1 font-mono text-xs">#{{ record.id }}</span>
        </template>
        <template v-else-if="column.key === 'cidr'"><span class="font-mono">{{ record.cidr }}</span></template>
        <template v-else-if="column.key === 'range'">
          <span class="font-mono text-xs">{{ record.range_start }} – {{ record.range_end }}</span>
        </template>
        <template v-else-if="column.key === 'gateway'"><span class="font-mono">{{ record.gateway ?? '—' }}</span></template>
        <template v-else-if="column.key === 'dns'">
          <span class="font-mono text-xs">{{ record.dns_servers.length > 0 ? record.dns_servers.join(', ') : '—' }}</span>
        </template>
        <template v-else-if="column.key === 'status'"><StatusTag kind="poolStatus" :value="record.status" /></template>
        <template v-else-if="column.key === 'op'">
          <Space :size="0">
            <Button size="small" type="link" @click="openAddresses(record as NetworkPoolView)">地址</Button>
            <Button size="small" type="link" @click="openDrawer(record as NetworkPoolView)">编辑</Button>
          </Space>
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
    <PoolDrawer v-model:open="drawerOpen" :pool="editing" @saved="list.load" />
    <PoolAddresses v-model:open="addressesOpen" :pool="addressesPool" @changed="list.load" />
  </div>
</template>
