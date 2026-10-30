<script lang="ts" setup>
import type { NetworkBinding, NetworkPool } from '#/api/types';

import { computed, onMounted, ref, shallowRef } from 'vue';

import { Table } from 'ant-design-vue';

import { listAll, networkApi } from '#/api/backend';
import CursorPager from '#/components/cursor-pager.vue';
import StatusTag from '#/components/status-tag.vue';
import TimeText from '#/components/time-text.vue';
import { errorToast, useCursorList } from '#/composables/api';

const props = defineProps<{ nodeId: string }>();

const filters = ref({ node_id: props.nodeId });
const list = useCursorList<NetworkBinding>(networkApi.bindings, filters);

const pools = shallowRef<NetworkPool[]>([]);
const poolById = computed(() => new Map(pools.value.map((p) => [p.id, p])));

onMounted(async () => {
  void list.load();
  try {
    pools.value = await listAll(networkApi.pools);
  } catch (error) {
    errorToast(error);
  }
});

const columns = [
  { dataIndex: 'id', title: '绑定 ID', width: 80 },
  { key: 'pool', title: '地址池' },
  { key: 'mode', title: '模式', width: 120 },
  { key: 'target', title: '父网卡 / Incus 网络' },
  { dataIndex: 'project_name', title: '项目' },
  { key: 'status', title: '状态', width: 80 },
  { key: 'validation', title: '验证', width: 100 },
  { key: 'validated', title: '验证时间', width: 120 },
];
</script>

<template>
  <div>
    <div class="mb-2 text-sm">
      只读。创建、验证、启用绑定请到
      <RouterLink :to="{ path: '/admin/network', query: { tab: 'bindings', node_id: nodeId } }">网络与 IP</RouterLink>。
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
        <template v-if="column.key === 'pool'">
          {{ poolById.get(record.network_pool_id)?.name ?? `#${record.network_pool_id}` }}
          <span class="text-muted-foreground ml-1 font-mono text-xs">{{ poolById.get(record.network_pool_id)?.cidr }}</span>
        </template>
        <template v-else-if="column.key === 'mode'"><StatusTag kind="networkMode" :value="record.mode" /></template>
        <template v-else-if="column.key === 'target'">
          <span class="font-mono text-xs">{{ record.parent_interface ?? record.incus_network_name ?? '—' }}</span>
        </template>
        <template v-else-if="column.key === 'status'"><StatusTag kind="simpleStatus" :value="record.status" /></template>
        <template v-else-if="column.key === 'validation'">
          <StatusTag kind="validation" :value="record.validation_status" />
        </template>
        <template v-else-if="column.key === 'validated'"><TimeText :value="record.validated_at" /></template>
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
  </div>
</template>
