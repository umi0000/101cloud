<script lang="ts" setup>
import type { NetworkPoolView } from './network';

import type { IPAddress } from '#/api/types';

import { computed, ref, watch } from 'vue';

import { Alert, Button, Card, Drawer, Input, message, Select, Space, Table, Textarea } from 'ant-design-vue';

import { networkApi } from '#/api/backend';
import CursorPager from '#/components/cursor-pager.vue';
import StatusTag from '#/components/status-tag.vue';
import { errorToast, useAction, useCursorList, useResource } from '#/composables/api';
import { ENUMS } from '#/locales/enums';

import { expandIPv4, MAX_ADDRESSES, parseAddressLines } from './network';

const props = defineProps<{ pool: NetworkPoolView | null }>();
const emit = defineEmits<{ changed: [] }>();
const open = defineModel<boolean>('open', { default: false });

const poolId = computed(() => props.pool?.id ?? '');
const detail = useResource(() => networkApi.pool(poolId.value));
const filters = ref<{ status: string | undefined }>({ status: undefined });
const list = useCursorList<IPAddress>((q) => networkApi.addresses(poolId.value, q), filters);
const statusOptions = Object.entries(ENUMS.addressStatus ?? {}).map(([value, l]) => ({ label: l.text, value }));

async function loadPool() {
  try {
    await detail.reload();
  } catch (error) {
    errorToast(error);
  }
}

/* ---------- 登记地址 ---------- */
const text = ref('');
const rangeStart = ref('');
const rangeEnd = ref('');
const addresses = computed(() => parseAddressLines(text.value));
const duplicates = computed(() => addresses.value.length - new Set(addresses.value).size);
const countValid = computed(() => addresses.value.length >= 1 && addresses.value.length <= MAX_ADDRESSES);
const action = useAction();
watch(text, () => action.resetKey());

watch(open, (v) => {
  if (!v || !props.pool) return;
  text.value = '';
  rangeStart.value = '';
  rangeEnd.value = '';
  // 替换筛选对象会触发 useCursorList 回到第一页并加载。
  filters.value = { status: undefined };
  void loadPool();
});

function expand() {
  try {
    text.value = expandIPv4(rangeStart.value.trim(), rangeEnd.value.trim()).join('\n');
  } catch (error) {
    message.warning(error instanceof Error ? error.message : String(error));
  }
}

async function submit() {
  if (!props.pool || !countValid.value || duplicates.value > 0) return;
  const batch = addresses.value;
  const r = await action.run((key) => networkApi.addAddresses(poolId.value, batch, { ifMatch: detail.etag.value, key }), {
    onConflict: loadPool,
    success: `已登记 ${batch.length} 个地址`,
  });
  if (!r) return;
  text.value = '';
  await loadPool();
  void list.load();
  emit('changed');
}

const columns = [
  { key: 'address', title: '地址' },
  { key: 'status', title: '状态', width: 90 },
  { key: 'instance', title: '实例', width: 110 },
  { key: 'node', title: '节点', width: 100 },
  { key: 'task', title: '预留任务', width: 100 },
];
</script>

<template>
  <Drawer v-model:open="open" destroy-on-close :title="`地址 · ${pool?.name ?? ''}`" :width="760">
    <div v-if="pool" class="text-muted-foreground mb-3 font-mono text-xs">
      {{ pool.cidr }} · 分配范围 {{ pool.range_start }} – {{ pool.range_end }} · 网关 {{ pool.gateway ?? '—' }}
    </div>
    <Card class="mb-3" size="small" title="登记地址">
      <Space class="mb-2" wrap>
        <Input v-model:value="rangeStart" class="w-40 font-mono" placeholder="起始 IPv4" />
        <span>–</span>
        <Input v-model:value="rangeEnd" class="w-40 font-mono" placeholder="结束 IPv4" />
        <Button @click="expand">按起止地址展开</Button>
      </Space>
      <Textarea
        v-model:value="text"
        :auto-size="{ minRows: 4, maxRows: 12 }"
        class="font-mono"
        placeholder="每行一个地址"
      />
      <div class="mt-2 flex items-center justify-between gap-2">
        <span class="text-sm">
          共 <b class="tabular-nums">{{ addresses.length }}</b> 个
          <template v-if="addresses.length > 0">
            （<span class="font-mono">{{ addresses[0] }}</span> … <span class="font-mono">{{ addresses[addresses.length - 1] }}</span>）
          </template>
        </span>
        <Button
          :disabled="!countValid || duplicates > 0 || !detail.etag.value"
          :loading="action.pending.value"
          type="primary"
          @click="submit"
        >
          确认登记
        </Button>
      </div>
      <Alert
        v-if="duplicates > 0"
        class="mt-2"
        :message="`有 ${duplicates} 个重复地址，请去重后提交`"
        show-icon
        type="warning"
      />
      <Alert
        v-else-if="addresses.length > MAX_ADDRESSES"
        class="mt-2"
        :message="`一次最多 ${MAX_ADDRESSES} 个地址`"
        show-icon
        type="warning"
      />
      <div class="text-muted-foreground mt-2 text-xs">
        地址须在分配范围内且不是网关；有任何非法、重复或已登记的地址时后端整批拒绝。
      </div>
    </Card>

    <Space class="mb-2">
      <Select v-model:value="filters.status" allow-clear class="w-32" :options="statusOptions" placeholder="状态" />
    </Space>
    <Table
      :columns="columns"
      :data-source="list.items.value"
      :loading="list.loading.value"
      :pagination="false"
      row-key="id"
      size="small"
    >
      <template #bodyCell="{ column, record }">
        <template v-if="column.key === 'address'"><span class="font-mono">{{ record.address }}</span></template>
        <template v-else-if="column.key === 'status'"><StatusTag kind="addressStatus" :value="record.status" /></template>
        <template v-else-if="column.key === 'instance'">
          <RouterLink v-if="record.instance_id" :to="`/admin/instances/${record.instance_id}`">#{{ record.instance_id }}</RouterLink>
          <span v-else>—</span>
        </template>
        <template v-else-if="column.key === 'node'">
          <RouterLink v-if="record.node_id" :to="`/admin/nodes/${record.node_id}`">#{{ record.node_id }}</RouterLink>
          <span v-else>—</span>
        </template>
        <template v-else-if="column.key === 'task'">
          <span class="font-mono">{{ record.reservation_task_id ? `#${record.reservation_task_id}` : '—' }}</span>
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
  </Drawer>
</template>
