<script lang="ts" setup>
import type { Instance } from '#/api/types';

import { onMounted, ref } from 'vue';

import { Table } from 'ant-design-vue';

import { instancesApi } from '#/api/backend';
import CursorPager from '#/components/cursor-pager.vue';
import StatusTag from '#/components/status-tag.vue';
import TimeText from '#/components/time-text.vue';
import { useCursorList } from '#/composables/api';
import { instanceState } from '#/locales/enums';
import { formatBytes } from '#/utils/format';

const props = defineProps<{ userId: string }>();

const filters = ref({ owner_user_id: props.userId });
const list = useCursorList<Instance>(instancesApi.list, filters);

const columns = [
  { key: 'name', title: '名称' },
  { key: 'state', title: '状态', width: 120 },
  { key: 'product', title: '类型', width: 90 },
  { key: 'kind', title: '形态', width: 90 },
  { key: 'resources', title: '规格' },
  { key: 'node', title: '节点', width: 90 },
  { key: 'created', title: '创建时间', width: 150 },
];

onMounted(() => list.load());
</script>

<template>
  <div>
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
          <RouterLink :to="`/admin/instances/${record.id}`">{{ record.name }}</RouterLink>
          <span class="text-muted-foreground ml-1 font-mono text-xs">#{{ record.id }}</span>
        </template>
        <template v-else-if="column.key === 'state'"><StatusTag :label="instanceState(record as Instance)" /></template>
        <template v-else-if="column.key === 'product'">
          <StatusTag kind="productType" :value="record.product_type" />
        </template>
        <template v-else-if="column.key === 'kind'">
          <StatusTag kind="instanceType" :value="record.instance_type" />
        </template>
        <template v-else-if="column.key === 'resources'">
          <span class="font-mono text-xs">
            {{ record.resources.vcpus }} vCPU / {{ formatBytes(record.resources.memory_bytes) }} /
            {{ formatBytes(record.resources.disk_bytes) }}
          </span>
        </template>
        <template v-else-if="column.key === 'node'">
          <RouterLink class="font-mono" :to="`/admin/nodes/${record.node_id}`">#{{ record.node_id }}</RouterLink>
        </template>
        <template v-else-if="column.key === 'created'"><TimeText :value="record.created_at" /></template>
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
