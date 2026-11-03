<script lang="ts" setup>
import type { CatalogRefs } from '../refs';

import type { InstanceType, Plan } from '#/api/types';

import { onMounted, ref } from 'vue';

import { Button, Select, Table } from 'ant-design-vue';

import { catalogApi } from '#/api/backend';
import { revisionTag } from '#/api/request';
import ConfirmAction from '#/components/confirm-action.vue';
import CursorPager from '#/components/cursor-pager.vue';
import StatusTag from '#/components/status-tag.vue';
import TimeText from '#/components/time-text.vue';
import { useAction, useCursorList } from '#/composables/api';
import { ENUMS } from '#/locales/enums';
import { formatBytes, formatPt } from '#/utils/format';

import PlanDrawer from './plan-drawer.vue';

const props = defineProps<{ refs: CatalogRefs }>();

const typeOptions = Object.entries(ENUMS.instanceType ?? {}).map(([value, l]) => ({ label: l.text, value }));

const filters = ref<{ instance_type?: InstanceType }>({});
const list = useCursorList<Plan, typeof filters.value>(catalogApi.plans, filters);
const action = useAction();

const columns = [
  { dataIndex: 'id', title: 'ID', width: 60 },
  { key: 'code', title: '代码 / 版本' },
  { dataIndex: 'name', title: '名称' },
  { key: 'type', title: '类型', width: 80 },
  { key: 'resources', title: '基准资源' },
  { key: 'nodes', title: '可用节点' },
  { key: 'gpu', title: 'GPU / 策略' },
  { key: 'status', title: '状态', width: 80 },
  { key: 'updated', title: '更新', width: 120 },
  { key: 'actions', title: '', width: 250 },
];

const drawerOpen = ref(false);
const drawerMode = ref<'create' | 'edit' | 'view'>('create');
const drawerSource = ref<null | Plan>(null);

function openDrawer(mode: 'create' | 'edit' | 'view', plan: null | Plan) {
  drawerMode.value = mode;
  drawerSource.value = plan;
  drawerOpen.value = true;
}

function gpuSummary(p: Plan): string {
  if (!p.gpu.model) return '不提供';
  const optional = p.allowed_overrides.gpu_count;
  const count = p.gpu.count > 0 ? `${p.gpu.count} 张` : '默认不带卡';
  return `${p.gpu.model} · ${count}${optional ? `（可选 ${optional.min}–${optional.max}）` : ''}`;
}

function gpuPolicySummary(p: Plan): string {
  if (p.instance_type === 'virtual-machine') return '不支持 GPU 策略';
  const { idle, rebalance } = p.gpu_policy;
  const idleText = idle.enabled ? `空闲 ${idle.timeout_seconds} 秒后${idle.action === 'detach_gpu' ? '移除 GPU' : '停止实例'}` : '空闲回收关闭';
  return `${idleText}；均衡${rebalance.enabled ? '开启' : '关闭'}${rebalance.allow_live ? ' · 允许运行中换卡' : ''}；上限 ${formatPt(rebalance.max_hourly_pt)} pt/h`;
}

const enableTarget = ref<null | Plan>(null);
const enableOpen = ref(false);
function askEnable(p: Plan) {
  enableTarget.value = p;
  enableOpen.value = true;
}
async function enable() {
  const p = enableTarget.value;
  if (!p) return false;
  const r = await action.run((key) => catalogApi.enablePlan(p.id, { ifMatch: revisionTag(p.revision), key }), {
    onConflict: () => list.load(),
    success: '套餐已启用',
  });
  await list.load();
  return !!r;
}

const disableTarget = ref<null | Plan>(null);
const disableOpen = ref(false);
function askDisable(p: Plan) {
  disableTarget.value = p;
  disableOpen.value = true;
}
async function disable() {
  const p = disableTarget.value;
  if (!p) return false;
  const r = await action.run((key) => catalogApi.disablePlan(p.id, { ifMatch: revisionTag(p.revision), key }), {
    onConflict: () => list.load(),
    success: '套餐已停用',
  });
  await list.load();
  return !!r;
}

onMounted(() => list.load());
</script>

<template>
  <div>
    <div class="mb-3 flex flex-wrap items-center gap-2">
      <Button type="primary" @click="openDrawer('create', null)">新建套餐</Button>
      <Select v-model:value="filters.instance_type" allow-clear class="!w-28" :options="typeOptions" placeholder="类型" />
      <span class="text-muted-foreground text-xs">多个套餐可同时启用；所有套餐均可随时编辑，修改仅用于后续创建，已有实例保持创建时配置。</span>
    </div>
    <Table
      :columns="columns"
      :data-source="list.items.value"
      :loading="list.loading.value"
      :pagination="false"
      row-key="id"
      :scroll="{ x: 1200 }"
      size="small"
    >
      <template #bodyCell="{ column, record }">
        <template v-if="column.dataIndex === 'id'"><span class="font-mono">{{ record.id }}</span></template>
        <template v-else-if="column.key === 'code'">
          <span class="font-mono">{{ record.code }} v{{ record.version }}</span>
        </template>
        <template v-else-if="column.key === 'type'"><StatusTag kind="instanceType" :value="record.instance_type" /></template>
        <template v-else-if="column.key === 'resources'">
          {{ record.resources.vcpus }} 核 / {{ formatBytes(record.resources.memory_bytes) }} /
          {{ formatBytes(record.resources.disk_bytes) }}
        </template>
        <template v-else-if="column.key === 'nodes'">
          <div v-for="id in (record as Plan).node_ids" :key="id">{{ refs.nodeName(id) }}</div>
          <span v-if="(record as Plan).node_ids.length === 0" class="text-muted-foreground">未配置</span>
        </template>
        <template v-else-if="column.key === 'gpu'">
          <div>{{ gpuSummary(record as Plan) }}</div>
          <div class="text-muted-foreground text-xs">{{ gpuPolicySummary(record as Plan) }}</div>
        </template>
        <template v-else-if="column.key === 'status'"><StatusTag kind="planStatus" :value="record.status" /></template>
        <template v-else-if="column.key === 'updated'"><TimeText :value="record.updated_at" /></template>
        <template v-else-if="column.key === 'actions'">
          <Button size="small" type="link" @click="openDrawer('view', record as Plan)">查看</Button>
          <Button size="small" type="link" @click="openDrawer('edit', record as Plan)">编辑</Button>
          <Button size="small" type="link" @click="openDrawer('create', record as Plan)">复制</Button>
          <Button v-if="record.status === 'disabled'" size="small" type="link" @click="askEnable(record as Plan)">启用</Button>
          <Button v-else-if="record.status === 'active'" danger size="small" type="link" @click="askDisable(record as Plan)">
            停用
          </Button>
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

    <PlanDrawer v-model:open="drawerOpen" :mode="drawerMode" :refs="props.refs" :source="drawerSource" @saved="list.load" />

    <ConfirmAction
      v-model:open="enableOpen"
      description="启用前校验：所选节点可用且各有兼容的已验证网络；镜像在每个节点/项目就绪；GPU 策略符合绑定政策；引用的政策、镜像、模板均已发布。仅启用此套餐，不会停用其他套餐；启用后仍可编辑。"
      :on-confirm="enable"
      :title="`启用套餐 ${enableTarget?.code ?? ''} v${enableTarget?.version ?? ''}`"
    />
    <ConfirmAction
      v-model:open="disableOpen"
      danger
      description="停用后用户不能再用此套餐创建实例；其他套餐和已有实例不受影响。停用后仍可编辑或重新启用。"
      :on-confirm="disable"
      :title="`停用套餐 ${disableTarget?.code ?? ''} v${disableTarget?.version ?? ''}`"
    />
  </div>
</template>
