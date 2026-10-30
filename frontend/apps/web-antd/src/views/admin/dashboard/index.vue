<script lang="ts" setup>
import type {
  AuditLog,
  CreditEnforcement,
  Instance,
  Node,
  NodeGPU,
  NodeResources,
  Page as PageOf,
  Task,
} from '#/api/types';

import { computed, onMounted, ref, shallowRef } from 'vue';

import { Page } from '@vben/common-ui';

import { Alert, Button, Card, Empty, Progress, Space, Table } from 'ant-design-vue';

import { financeApi, instancesApi, listAll, nodesApi, opsApi } from '#/api/backend';
import StatusTag from '#/components/status-tag.vue';
import TimeText from '#/components/time-text.vue';
import UserName from '#/components/user-name.vue';
import { usePolling } from '#/composables/api';
import { AUDIT_EVENTS, TASK_TYPES } from '#/locales/enums';
import { formatMib, formatTime, toShanghaiISO } from '#/utils/format';

import NodeCard from './components/node-card.vue';

/** gpus/resources 为 null 表示该节点的请求失败（显示"未知"）。 */
interface NodeView {
  gpus: NodeGPU[] | null;
  node: Node;
  resources: NodeResources | null;
}

const nodes = shallowRef<null | PageOf<NodeView>>(null);
const running = shallowRef<null | PageOf<Instance>>(null);
const unknownTasks = shallowRef<null | PageOf<Task>>(null);
const grace = shallowRef<null | PageOf<CreditEnforcement>>(null);
const audit = shallowRef<AuditLog[] | null>(null);
const failed = ref<string[]>([]);
const loading = ref(false);
const refreshedAt = ref<null | string>(null);

async function loadNodes(): Promise<PageOf<NodeView>> {
  const r = await nodesApi.list({ limit: 100 });
  const items = await Promise.all(
    r.data.items.map(async (node): Promise<NodeView> => {
      const [res, gpus] = await Promise.allSettled([nodesApi.resources(node.id), listAll((q) => nodesApi.gpus(node.id, q))]);
      return {
        gpus: gpus.status === 'fulfilled' ? gpus.value : null,
        node,
        resources: res.status === 'fulfilled' ? res.value.data : null,
      };
    }),
  );
  return { items, next_cursor: r.data.next_cursor };
}

/** 各区块独立加载；失败的区块保留上次数据并在顶部提示。 */
async function refresh() {
  if (loading.value) return;
  loading.value = true;
  const jobs: [string, () => Promise<void>][] = [
    ['节点', async () => void (nodes.value = await loadNodes())],
    ['运行中实例', async () => void (running.value = (await instancesApi.list({ limit: 100, observed_state: 'running' })).data)],
    ['结果未知任务', async () => void (unknownTasks.value = (await opsApi.tasks({ limit: 100, status: 'unknown' })).data)],
    ['额度宽限', async () => void (grace.value = (await financeApi.enforcements({ limit: 100, status: 'grace' })).data)],
    ['审计', async () => void (audit.value = (await opsApi.audit({ limit: 8 })).data.items)],
  ];
  const results = await Promise.allSettled(jobs.map(([, run]) => run()));
  failed.value = jobs.filter((_, i) => results[i]?.status === 'rejected').map(([label]) => label);
  refreshedAt.value = toShanghaiISO(new Date());
  loading.value = false;
}

/** 单页最多 100 条；有下一页显示 "100+"。 */
function count(p: null | PageOf<unknown>): string {
  if (!p) return '未知';
  return p.next_cursor ? `${p.items.length}+` : String(p.items.length);
}
const positive = (p: null | PageOf<unknown>) => !!p && p.items.length > 0;

function tally<K extends string>(values: K[]): [K, number][] {
  const m = new Map<K, number>();
  for (const v of values) m.set(v, (m.get(v) ?? 0) + 1);
  return [...m.entries()];
}
const nodeAdmin = computed(() => tally((nodes.value?.items ?? []).map((v) => v.node.administrative_state)));
const nodeHealth = computed(() => tally((nodes.value?.items ?? []).map((v) => v.node.health)));

/** 名额 = 所有卡 allocation_count 合计 / status=available 卡的 max_allocations 合计。 */
const gpuSlots = computed(() => {
  if (!nodes.value) return null;
  let used = 0;
  let capacity = 0;
  let unhealthy = 0;
  let cards = 0;
  let missingNodes = 0;
  for (const v of nodes.value.items) {
    if (!v.gpus) {
      missingNodes++;
      continue;
    }
    for (const g of v.gpus) {
      cards++;
      used += g.allocation_count;
      if (g.status === 'available') capacity += g.max_allocations;
      if (g.health !== 'healthy') unhealthy++;
    }
  }
  return { capacity, cards, missingNodes, unhealthy, used };
});

type GpuRow = NodeGPU & { node_name: string };
const gpuRows = computed<GpuRow[]>(() =>
  (nodes.value?.items ?? []).flatMap((v) => (v.gpus ?? []).map((g) => ({ ...g, node_name: v.node.name }))),
);
const gpuColumns = [
  { key: 'node', title: '节点' },
  { key: 'model', title: '型号' },
  { key: 'util', title: 'SM 利用率', width: 160 },
  { key: 'mem', title: '显存', width: 170 },
  { key: 'alloc', title: '分配', width: 80 },
  { key: 'health', title: '健康', width: 90 },
  { key: 'status', title: '状态', width: 90 },
];

const auditColumns = [
  { key: 'time', title: '时间', width: 120 },
  { key: 'actor', title: '操作者' },
  { key: 'event', title: '事件' },
  { key: 'resource', title: '资源' },
  { dataIndex: 'result', title: '结果', width: 80 },
];

const PENDING_SHOWN = 5;

usePolling(refresh, () => 30_000);
onMounted(refresh);
</script>

<template>
  <Page title="管理概览">
    <template #extra>
      <Space>
        <span v-if="refreshedAt" class="text-muted-foreground text-xs">每 30 秒刷新 · 上次 {{ formatTime(refreshedAt) }}</span>
        <Button :loading="loading" size="small" @click="refresh">刷新</Button>
      </Space>
    </template>

    <Alert
      v-if="failed.length > 0"
      class="mb-3"
      :message="`部分数据加载失败：${failed.join('、')}。显示的是上次成功获取的数据。`"
      show-icon
      type="warning"
    />

    <div class="grid grid-cols-1 gap-3 sm:grid-cols-2 xl:grid-cols-5">
      <Card size="small">
        <div class="text-muted-foreground text-xs">节点</div>
        <div class="text-2xl font-semibold tabular-nums">{{ count(nodes) }}</div>
        <div class="mt-1 flex flex-wrap gap-1">
          <span v-for="[k, n] in nodeAdmin" :key="k"><StatusTag kind="adminState" :value="k" /> {{ n }}</span>
        </div>
        <div class="mt-1 flex flex-wrap gap-1">
          <span v-for="[k, n] in nodeHealth" :key="k"><StatusTag kind="health" :value="k" /> {{ n }}</span>
        </div>
      </Card>
      <Card size="small">
        <div class="text-muted-foreground text-xs">运行中实例</div>
        <RouterLink class="text-2xl font-semibold tabular-nums" to="/admin/instances">{{ count(running) }}</RouterLink>
      </Card>
      <Card size="small">
        <div class="text-muted-foreground text-xs">GPU 名额（已分配 / 可用卡上限）</div>
        <div class="text-2xl font-semibold tabular-nums">
          <template v-if="gpuSlots">{{ gpuSlots.used }} / {{ gpuSlots.capacity }}</template>
          <template v-else>未知</template>
        </div>
        <div v-if="gpuSlots" class="mt-1 text-xs">
          <span class="text-muted-foreground">{{ gpuSlots.cards }} 张卡</span>
          <span v-if="gpuSlots.unhealthy > 0" class="ml-2 text-orange-500">{{ gpuSlots.unhealthy }} 张不健康</span>
          <span v-if="gpuSlots.missingNodes > 0" class="ml-2 text-orange-500">
            {{ gpuSlots.missingNodes }} 个节点 GPU 未知
          </span>
        </div>
      </Card>
      <Card size="small">
        <div class="text-muted-foreground text-xs">结果未知的任务</div>
        <RouterLink
          class="text-2xl font-semibold tabular-nums"
          :class="positive(unknownTasks) ? 'text-orange-500' : ''"
          :to="{ path: '/admin/tasks', query: { status: 'unknown' } }"
        >
          {{ count(unknownTasks) }}
        </RouterLink>
      </Card>
      <Card size="small">
        <div class="text-muted-foreground text-xs">额度宽限中</div>
        <RouterLink
          class="text-2xl font-semibold tabular-nums"
          :class="positive(grace) ? 'text-red-500' : ''"
          to="/admin/finance"
        >
          {{ count(grace) }}
        </RouterLink>
      </Card>
    </div>

    <div v-if="nodes?.next_cursor" class="mt-2 text-xs text-orange-500">
      节点超过 100 个，以下节点卡片与 GPU 统计只包含前 100 个。
    </div>

    <h3 class="mb-2 mt-4 font-medium">节点</h3>
    <Empty v-if="nodes && nodes.items.length === 0" description="暂无节点" />
    <div v-else class="grid gap-3 md:grid-cols-2 xl:grid-cols-3">
      <NodeCard v-for="v in nodes?.items ?? []" :key="v.node.id" :node="v.node" :resources="v.resources" />
    </div>

    <Card class="mt-4" size="small" title="GPU 负载">
      <Table
        :columns="gpuColumns"
        :data-source="gpuRows"
        :loading="loading && !nodes"
        :pagination="false"
        row-key="id"
        size="small"
      >
        <template #bodyCell="{ column, record }">
          <template v-if="column.key === 'node'">
            <RouterLink :to="`/admin/nodes/${record.node_id}`">{{ record.node_name }}</RouterLink>
          </template>
          <template v-else-if="column.key === 'model'">
            {{ record.model }}
            <div class="text-muted-foreground font-mono text-xs">{{ record.pci_address }}</div>
          </template>
          <template v-else-if="column.key === 'util'">
            <Progress
              v-if="typeof record.observation?.utilization_percent === 'number'"
              :percent="Math.round(record.observation.utilization_percent)"
              size="small"
            />
            <span v-else class="unknown-hatch">未知</span>
          </template>
          <template v-else-if="column.key === 'mem'">
            <span
              v-if="typeof record.observation?.memory_used_mib === 'number' && typeof record.observation.memory_total_mib === 'number'"
              class="font-mono text-xs"
            >
              {{ formatMib(record.observation.memory_used_mib) }} / {{ formatMib(record.observation.memory_total_mib) }}
            </span>
            <span v-else class="unknown-hatch">未知</span>
          </template>
          <template v-else-if="column.key === 'alloc'">
            <span class="font-mono">{{ record.allocation_count }} / {{ record.max_allocations }}</span>
          </template>
          <template v-else-if="column.key === 'health'"><StatusTag kind="gpuHealth" :value="record.health" /></template>
          <template v-else-if="column.key === 'status'"><StatusTag kind="gpuStatus" :value="record.status" /></template>
        </template>
      </Table>
    </Card>

    <div class="mt-4 grid gap-3 xl:grid-cols-2">
      <Card size="small" title="待处理">
        <div v-if="grace?.items.length" class="mb-3">
          <div class="mb-1 flex justify-between text-sm font-medium">
            <span>额度宽限中</span>
            <RouterLink class="text-xs" to="/admin/finance">全部</RouterLink>
          </div>
          <div v-for="e in grace.items.slice(0, PENDING_SHOWN)" :key="e.id" class="flex items-center gap-2 py-0.5 text-sm">
            <StatusTag kind="enforcement" :value="e.status" />
            <UserName :id="e.user_id" link />
            <span class="text-muted-foreground">停机期限</span>
            <TimeText :value="e.shutdown_deadline" />
            <RouterLink class="ml-auto text-xs" to="/admin/finance">处理</RouterLink>
          </div>
        </div>
        <div v-if="unknownTasks?.items.length">
          <div class="mb-1 flex justify-between text-sm font-medium">
            <span>结果未知任务</span>
            <RouterLink class="text-xs" :to="{ path: '/admin/tasks', query: { status: 'unknown' } }">全部</RouterLink>
          </div>
          <div
            v-for="t in unknownTasks.items.slice(0, PENDING_SHOWN)"
            :key="t.id"
            class="flex items-center gap-2 py-0.5 text-sm"
          >
            <span class="font-mono">#{{ t.id }}</span>
            <span>{{ TASK_TYPES[t.type] ?? t.type }}</span>
            <RouterLink v-if="t.instance_id" :to="`/admin/instances/${t.instance_id}`">实例 #{{ t.instance_id }}</RouterLink>
            <RouterLink class="ml-auto text-xs" :to="{ path: '/admin/tasks', query: { status: 'unknown' } }">
              核对
            </RouterLink>
          </div>
        </div>
        <Empty
          v-if="grace && unknownTasks && !grace.items.length && !unknownTasks.items.length"
          description="没有待处理事项"
        />
      </Card>

      <Card size="small" title="最近审计">
        <template #extra><RouterLink class="text-xs" to="/admin/audit">全部</RouterLink></template>
        <Table
          :columns="auditColumns"
          :data-source="audit ?? []"
          :pagination="false"
          row-key="id"
          size="small"
        >
          <template #bodyCell="{ column, record }">
            <template v-if="column.key === 'time'"><TimeText :value="record.occurred_at" /></template>
            <template v-else-if="column.key === 'actor'"><UserName :id="record.actor_user_id" link /></template>
            <template v-else-if="column.key === 'event'">{{ AUDIT_EVENTS[record.event_type] ?? record.event_type }}</template>
            <template v-else-if="column.key === 'resource'">
              <span class="font-mono text-xs">{{ record.resource_type }}{{ record.resource_id ? ` #${record.resource_id}` : '' }}</span>
            </template>
          </template>
        </Table>
      </Card>
    </div>
  </Page>
</template>

<style scoped>
/* 缺测：显示"未知"并画斜线。 */
.unknown-hatch {
  padding: 0 6px;
  font-size: 12px;
  color: rgb(128 128 128);
  background-image: repeating-linear-gradient(
    135deg,
    rgb(128 128 128 / 18%) 0,
    rgb(128 128 128 / 18%) 2px,
    transparent 2px,
    transparent 6px
  );
  border-radius: 2px;
}
</style>
