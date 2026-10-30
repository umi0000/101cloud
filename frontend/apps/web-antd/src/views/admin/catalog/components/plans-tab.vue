<script lang="ts" setup>
import type { CatalogRefs } from '../refs';

import type { InstanceType, Plan } from '#/api/types';

import { onMounted, ref } from 'vue';

import { Button, Select, Space, Table } from 'ant-design-vue';

import { catalogApi } from '#/api/backend';
import { revisionTag } from '#/api/request';
import ConfirmAction from '#/components/confirm-action.vue';
import CursorPager from '#/components/cursor-pager.vue';
import StatusTag from '#/components/status-tag.vue';
import TimeText from '#/components/time-text.vue';
import { useAction, useCursorList } from '#/composables/api';
import { ENUMS } from '#/locales/enums';
import { formatBytes } from '#/utils/format';

import PlanDrawer from './plan-drawer.vue';

const props = defineProps<{ refs: CatalogRefs }>();

const typeOptions = Object.entries(ENUMS.instanceType ?? {}).map(([value, l]) => ({ label: l.text, value }));

const filters = ref<{ instance_type?: InstanceType; zone_id?: string }>({});
const list = useCursorList<Plan, typeof filters.value>(catalogApi.plans, filters);
const action = useAction();

const columns = [
  { dataIndex: 'id', title: 'ID', width: 60 },
  { key: 'code', title: '代码 / 版本' },
  { dataIndex: 'name', title: '名称' },
  { key: 'zone', title: '可用区' },
  { key: 'type', title: '类型', width: 80 },
  { key: 'resources', title: '基准资源' },
  { key: 'gpu', title: 'GPU' },
  { key: 'status', title: '状态', width: 80 },
  { key: 'updated', title: '更新', width: 120 },
  { key: 'actions', title: '', width: 190 },
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

const publishTarget = ref<null | Plan>(null);
const publishOpen = ref(false);
function askPublish(p: Plan) {
  publishTarget.value = p;
  publishOpen.value = true;
}
async function publish() {
  const p = publishTarget.value;
  if (!p) return false;
  const r = await action.run((key) => catalogApi.publishPlan(p.id, { ifMatch: revisionTag(p.revision), key }), {
    onConflict: () => list.load(),
    success: '套餐已发布',
  });
  await list.load();
  return !!r;
}

const retireTarget = ref<null | Plan>(null);
const retireOpen = ref(false);
function askRetire(p: Plan) {
  retireTarget.value = p;
  retireOpen.value = true;
}
async function retire() {
  const p = retireTarget.value;
  if (!p) return false;
  const r = await action.run((key) => catalogApi.retirePlan(p.id, { ifMatch: revisionTag(p.revision), key }), {
    onConflict: () => list.load(),
    success: '套餐已退役',
  });
  await list.load();
  return !!r;
}

onMounted(() => list.load());
</script>

<template>
  <div>
    <Space class="mb-3" wrap>
      <Button type="primary" @click="openDrawer('create', null)">新建套餐</Button>
      <Select
        v-model:value="filters.zone_id"
        allow-clear
        class="w-44"
        :options="props.refs.zones.value.map((z) => ({ label: props.refs.zoneName(z.id), value: z.id }))"
        placeholder="可用区"
      />
      <Select v-model:value="filters.instance_type" allow-clear class="w-28" :options="typeOptions" placeholder="类型" />
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
        <template v-if="column.dataIndex === 'id'"><span class="font-mono">{{ record.id }}</span></template>
        <template v-else-if="column.key === 'code'">
          <span class="font-mono">{{ record.code }} v{{ record.version }}</span>
        </template>
        <template v-else-if="column.key === 'zone'">{{ props.refs.zoneName(record.zone_id) }}</template>
        <template v-else-if="column.key === 'type'"><StatusTag kind="instanceType" :value="record.instance_type" /></template>
        <template v-else-if="column.key === 'resources'">
          {{ record.resources.vcpus }} 核 / {{ formatBytes(record.resources.memory_bytes) }} /
          {{ formatBytes(record.resources.disk_bytes) }}
        </template>
        <template v-else-if="column.key === 'gpu'">{{ gpuSummary(record as Plan) }}</template>
        <template v-else-if="column.key === 'status'"><StatusTag kind="catalogStatus" :value="record.status" /></template>
        <template v-else-if="column.key === 'updated'"><TimeText :value="record.updated_at" /></template>
        <template v-else-if="column.key === 'actions'">
          <template v-if="record.status === 'draft'">
            <Button size="small" type="link" @click="openDrawer('edit', record as Plan)">编辑</Button>
            <Button size="small" type="link" @click="askPublish(record as Plan)">发布</Button>
          </template>
          <template v-else>
            <Button size="small" type="link" @click="openDrawer('view', record as Plan)">查看</Button>
            <Button size="small" type="link" @click="openDrawer('create', record as Plan)">新版本</Button>
            <Button v-if="record.status === 'active'" danger size="small" type="link" @click="askRetire(record as Plan)">
              退役
            </Button>
          </template>
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
      v-model:open="publishOpen"
      description="发布前后端校验：引用的政策、镜像、模板都已发布，可用区启用，网络已验证且属于该可用区。发布后套餐不能修改，只能退役。"
      :on-confirm="publish"
      :title="`发布套餐 ${publishTarget?.code ?? ''} v${publishTarget?.version ?? ''}`"
    />
    <ConfirmAction
      v-model:open="retireOpen"
      danger
      description="退役后用户不能再用此套餐创建实例；已有实例不受影响。"
      :on-confirm="retire"
      :title="`退役套餐 ${retireTarget?.code ?? ''} v${retireTarget?.version ?? ''}`"
    />
  </div>
</template>
