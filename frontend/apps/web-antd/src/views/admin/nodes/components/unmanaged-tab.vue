<script lang="ts" setup>
import type { Node } from '#/api/types';

import type { UnmanagedView } from './node-forms';

import { onMounted, ref, shallowRef, watch } from 'vue';

import { Button, Table, Tooltip } from 'ant-design-vue';

import { nodesApi } from '#/api/backend';
import CursorPager from '#/components/cursor-pager.vue';
import StatusTag from '#/components/status-tag.vue';
import TimeText from '#/components/time-text.vue';
import { useCursorList } from '#/composables/api';

import ImportDrawer from './import-drawer.vue';

const props = defineProps<{ epoch: number; etag: null | string; node: Node }>();
const emit = defineEmits<{ imported: [] }>();

const filters = ref({});
const list = useCursorList<UnmanagedView>((q) => nodesApi.unmanaged(props.node.id, q), filters);
onMounted(() => list.load());
watch(() => props.epoch, () => list.load());

const columns = [
  { key: 'name', title: 'Incus 名称' },
  { key: 'type', title: '类型', width: 80 },
  { dataIndex: 'architecture', title: '架构', width: 100 },
  { dataIndex: 'observed_state', title: '实际状态', width: 100 },
  { dataIndex: 'project_name', title: '项目' },
  { key: 'observed', title: '观测时间', width: 120 },
  { key: 'op', title: '', width: 80 },
];

const selected = shallowRef<null | UnmanagedView>(null);
const importOpen = ref(false);
function startImport(item: UnmanagedView) {
  selected.value = item;
  importOpen.value = true;
}
function onImported() {
  void list.load();
  emit('imported');
}
</script>

<template>
  <div>
    <div class="text-muted-foreground mb-2 text-xs">由探测库存发现、尚未纳管的 Incus 实例。纳管后从此列表消失。</div>
    <Table
      :columns="columns"
      :data-source="list.items.value"
      :loading="list.loading.value"
      :pagination="false"
      row-key="id"
      size="small"
    >
      <template #bodyCell="{ column, record }">
        <template v-if="column.key === 'name'"><span class="font-mono">{{ record.incus_name }}</span></template>
        <template v-else-if="column.key === 'type'"><StatusTag kind="instanceType" :value="record.instance_type" /></template>
        <template v-else-if="column.key === 'observed'"><TimeText :value="record.observed_at" /></template>
        <template v-else-if="column.key === 'op'">
          <Tooltip :title="record.incus_uuid ? undefined : 'Incus 未报告该实例 UUID，无法纳管'">
            <Button :disabled="!record.incus_uuid" size="small" type="link" @click="startImport(record as UnmanagedView)">纳管</Button>
          </Tooltip>
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
    <ImportDrawer v-model:open="importOpen" :etag="etag" :item="selected" :node="node" @imported="onImported" />
  </div>
</template>
