<script lang="ts" setup>
import type { Node, NodeGPU } from '#/api/types';

import { onMounted, ref, watch } from 'vue';

import { Button, Table, Tooltip } from 'ant-design-vue';

import { nodesApi } from '#/api/backend';
import CursorPager from '#/components/cursor-pager.vue';
import StatusTag from '#/components/status-tag.vue';
import TimeText from '#/components/time-text.vue';
import { useCursorList } from '#/composables/api';
import { formatMib, formatPercent, shortId } from '#/utils/format';

import GpuEditDrawer from './gpu-edit-drawer.vue';

const props = defineProps<{ epoch: number; node: Node }>();

const filters = ref({});
const list = useCursorList<NodeGPU>((q) => nodesApi.gpus(props.node.id, q), filters);

onMounted(() => {
  void list.load();
});
watch(() => props.epoch, () => list.load());


function memoryUse(g: NodeGPU) {
  const o = g.observation;
  if (!o || o.memory_used_mib === null || o.memory_total_mib === null) return '未知';
  return `${formatMib(o.memory_used_mib)} / ${formatMib(o.memory_total_mib)}`;
}

const columns = [
  { key: 'pci', title: 'PCI', width: 120 },
  { key: 'model', title: '型号 / 显存' },
  { key: 'uuid', title: 'UUID', width: 140 },
  { key: 'status', title: '状态', width: 80 },
  { key: 'health', title: '健康', width: 80 },
  { key: 'usage', title: 'SM / 显存占用' },
  { key: 'alloc', title: '分配 / 名额', width: 110 },
  { key: 'quality', title: '观测', width: 150 },
  { key: 'op', title: '', width: 70 },
];

const editId = ref<null | string>(null);
const editOpen = ref(false);
function edit(id: string) {
  editId.value = id;
  editOpen.value = true;
}
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
        <template v-if="column.key === 'pci'"><span class="font-mono text-xs">{{ record.pci_address }}</span></template>
        <template v-else-if="column.key === 'model'">
          {{ record.model }}
          <span class="text-muted-foreground ml-1 text-xs">{{ formatMib(record.memory_mib) }}</span>
        </template>
        <template v-else-if="column.key === 'uuid'">
          <Tooltip :title="record.uuid"><span class="font-mono text-xs">{{ shortId(record.uuid) }}</span></Tooltip>
        </template>
        <template v-else-if="column.key === 'status'"><StatusTag kind="gpuStatus" :value="record.status" /></template>
        <template v-else-if="column.key === 'health'">
          <StatusTag kind="gpuHealth" :value="record.health" />
          <Tooltip v-if="record.observation?.error" :title="record.observation.error">
            <span class="ml-1 cursor-help text-xs text-red-500">详情</span>
          </Tooltip>
        </template>
        <template v-else-if="column.key === 'usage'">
          <span class="tabular-nums">SM {{ formatPercent(record.observation?.utilization_percent) }}</span>
          <span class="text-muted-foreground mx-1">·</span>
          <span class="tabular-nums">{{ memoryUse(record as NodeGPU) }}</span>
        </template>
        <template v-else-if="column.key === 'alloc'">
          <span class="tabular-nums">{{ record.allocation_count }} / {{ record.max_allocations }}</span>
          <div v-if="record.reservation_count > 0" class="text-muted-foreground text-xs">预留 {{ record.reservation_count }}</div>
        </template>
        <template v-else-if="column.key === 'quality'">
          <StatusTag kind="quality" :value="record.quality" />
          <TimeText class="ml-1" :value="record.observed_at" />
        </template>
        <template v-else-if="column.key === 'op'">
          <Button size="small" type="link" @click="edit(record.id)">编辑</Button>
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
    <GpuEditDrawer v-model:open="editOpen" :gpu-id="editId" :node="node" @saved="list.load" />
  </div>
</template>
