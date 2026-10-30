<script lang="ts" setup>
import type { NetworkPoolView } from './network';

import type { NetworkBinding, Node } from '#/api/types';

import { computed, onMounted, ref } from 'vue';

import { Button, Select, Space, Table, Tooltip } from 'ant-design-vue';

import { networkApi } from '#/api/backend';
import { revisionTag } from '#/api/request';
import ConfirmAction from '#/components/confirm-action.vue';
import CursorPager from '#/components/cursor-pager.vue';
import StatusTag from '#/components/status-tag.vue';
import TimeText from '#/components/time-text.vue';
import { trackAccepted, useAction, useCursorList } from '#/composables/api';

import BindingDrawer from './binding-drawer.vue';

const props = defineProps<{ initialNodeId?: string; initialPoolId?: string; nodes: Node[]; pools: NetworkPoolView[] }>();

/** 筛选参数名为 pool_id；响应字段叫 network_pool_id。 */
const filters = ref<{ node_id: string | undefined; pool_id: string | undefined }>({
  node_id: props.initialNodeId,
  pool_id: props.initialPoolId,
});
const list = useCursorList<NetworkBinding>(networkApi.bindings, filters);
onMounted(() => list.load());

const nodeById = computed(() => new Map(props.nodes.map((n) => [n.id, n])));
const poolById = computed(() => new Map(props.pools.map((p) => [p.id, p])));

const columns = [
  { dataIndex: 'id', title: 'ID', width: 70 },
  { key: 'node', title: '节点' },
  { key: 'pool', title: '地址池' },
  { key: 'mode', title: '模式', width: 110 },
  { key: 'target', title: '父网卡 / Incus 网络' },
  { dataIndex: 'project_name', title: '项目' },
  { key: 'status', title: '状态', width: 70 },
  { key: 'validation', title: '验证', width: 90 },
  { key: 'validated', title: '验证时间', width: 110 },
  { key: 'op', title: '', width: 150 },
];

/* ---------- 行操作：验证 / 启用 / 停用，共用一个确认框 ---------- */
type OpKind = 'disable' | 'enable' | 'validate';
const op = ref<null | { binding: NetworkBinding; kind: OpKind }>(null);
const opOpen = computed({
  get: () => op.value !== null,
  set: (v) => {
    if (!v) op.value = null;
  },
});
const OP_TEXT: Record<OpKind, { description: string; title: string }> = {
  disable: { description: '停用后新实例不能再使用该网络；不影响已有实例。', title: '停用绑定' },
  enable: { description: '启用后该绑定作为网络选项提供给新实例。', title: '启用绑定' },
  validate: {
    description: '只读核对节点上的实际网络。验证期间绑定会先变为停用、待验证；完成后查看验证结果再启用。',
    title: '验证绑定',
  },
};
const opText = computed(() => (op.value ? OP_TEXT[op.value.kind] : { description: '', title: '' }));
const action = useAction();

function startOp(binding: NetworkBinding, kind: OpKind) {
  action.resetKey();
  op.value = { binding, kind };
}

async function confirmOp() {
  const current = op.value;
  if (!current) return false;
  const { binding, kind } = current;
  const w = { ifMatch: revisionTag(binding.revision) };
  if (kind === 'validate') {
    const r = await action.run((key) => networkApi.validateBinding(binding.id, { ...w, key }), {
      onConflict: list.load,
      success: '已提交验证',
    });
    if (!r) return false;
    trackAccepted(r.data, `验证网络绑定 #${binding.id}`, list.load);
  } else {
    const status = kind === 'enable' ? 'active' : 'disabled';
    const r = await action.run(() => networkApi.updateBinding(binding.id, status, w), {
      onConflict: list.load,
      success: kind === 'enable' ? '已启用' : '已停用',
    });
    if (!r) return false;
  }
  void list.load();
  return true;
}

const drawerOpen = ref(false);
const expanded = ref<string[]>([]);
function toggleDetails(id: string) {
  expanded.value = expanded.value.includes(id) ? expanded.value.filter((k) => k !== id) : [...expanded.value, id];
}
</script>

<template>
  <div>
    <div class="mb-3 flex flex-wrap items-center justify-between gap-2">
      <Space wrap>
        <Select
          v-model:value="filters.node_id"
          allow-clear
          class="w-44"
          option-filter-prop="label"
          :options="nodes.map((n) => ({ label: n.name, value: n.id }))"
          placeholder="节点"
          show-search
        />
        <Select
          v-model:value="filters.pool_id"
          allow-clear
          class="w-44"
          :options="pools.map((p) => ({ label: p.name, value: p.id }))"
          placeholder="地址池"
        />
      </Space>
      <Button type="primary" @click="drawerOpen = true">创建绑定</Button>
    </div>
    <Table
      v-model:expanded-row-keys="expanded"
      :columns="columns"
      :data-source="list.items.value"
      :expand-icon-column-index="-1"
      :loading="list.loading.value"
      :pagination="false"
      row-key="id"
      size="small"
    >
      <template #bodyCell="{ column, record }">
        <template v-if="column.key === 'node'">
          <RouterLink :to="`/admin/nodes/${record.node_id}`">
            {{ nodeById.get(record.node_id)?.name ?? `#${record.node_id}` }}
          </RouterLink>
        </template>
        <template v-else-if="column.key === 'pool'">
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
        <template v-else-if="column.key === 'op'">
          <Space :size="0">
            <Button size="small" type="link" @click="startOp(record as NetworkBinding, 'validate')">验证</Button>
            <Tooltip v-if="record.status !== 'active'" :title="record.validation_status === 'verified' ? undefined : '需先验证通过'">
              <Button
                :disabled="record.validation_status !== 'verified'"
                size="small"
                type="link"
                @click="startOp(record as NetworkBinding, 'enable')"
              >
                启用
              </Button>
            </Tooltip>
            <Button v-else size="small" type="link" @click="startOp(record as NetworkBinding, 'disable')">停用</Button>
            <Button size="small" type="link" @click="toggleDetails(record.id)">详情</Button>
          </Space>
        </template>
      </template>
      <template #expandedRowRender="{ record }">
        <div class="text-muted-foreground mb-1 text-xs">验证详情（原样显示）· MTU {{ record.mtu ?? '—' }}</div>
        <pre class="m-0 max-h-80 overflow-auto whitespace-pre-wrap font-mono text-xs">{{
          record.validation_details ? JSON.stringify(record.validation_details, null, 2) : '—'
        }}</pre>
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
    <ConfirmAction
      v-model:open="opOpen"
      :description="opText.description"
      :on-confirm="confirmOp"
      :title="opText.title"
    />
    <BindingDrawer
      v-model:open="drawerOpen"
      :initial-node-id="filters.node_id"
      :nodes="nodes"
      :pools="pools"
      @created="list.load"
    />
  </div>
</template>
