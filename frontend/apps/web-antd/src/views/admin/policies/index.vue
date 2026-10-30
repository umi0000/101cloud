<script lang="ts" setup>
/** 政策与定价（前端设计 7.8）：左侧 6 类政策，右侧版本列表；详情抽屉中编辑/发布草稿、退役已发布版本。 */
import type { PolicyKind, PolicyVersion } from '#/api/types';

import { computed, onMounted, ref, watch } from 'vue';

import { Page } from '@vben/common-ui';

import { Button, Card, Descriptions, DescriptionsItem, Drawer, Menu, MenuItem, Space, Table, Tag, Tooltip } from 'ant-design-vue';

import { listAll, nodesApi, policiesApi } from '#/api/backend';
import { POLICY_KINDS } from '#/api/types';
import ConfirmAction from '#/components/confirm-action.vue';
import CursorPager from '#/components/cursor-pager.vue';
import StatusTag from '#/components/status-tag.vue';
import TimeText from '#/components/time-text.vue';
import UserName from '#/components/user-name.vue';
import { errorToast, useAction, useCursorList, useResource } from '#/composables/api';
import { POLICY_KIND_NAMES } from '#/locales/enums';

import BindingsCard from './components/bindings-card.vue';
import { configSummary, displayValue, POLICY_FIELDS, rawValue, versionLabel } from './components/fields';
import PublishModal from './components/publish-modal.vue';
import VersionEditor from './components/version-editor.vue';

const kind = ref<PolicyKind>('pricing');
const menuKeys = computed({
  get: () => [kind.value],
  set: (keys: (number | string)[]) => {
    const k = POLICY_KINDS.find((x) => x === keys[0]);
    if (k) kind.value = k;
  },
});

/* 版本列表（游标分页）。后端列表只接受 limit/cursor。 */
const list = useCursorList<PolicyVersion>((q) => policiesApi.list(kind.value, q), ref({}));
watch(kind, () => list.reset());

/* 每类的全部版本：用于显示 `code v{version}`、绑定下拉和发布对比。 */
const allVersions = ref<Partial<Record<PolicyKind, PolicyVersion[]>>>({});
async function loadVersions(k: PolicyKind) {
  try {
    allVersions.value = { ...allVersions.value, [k]: await listAll((q) => policiesApi.list(k, q)) };
  } catch (error) {
    errorToast(error);
  }
}

const bindings = useResource(() => policiesApi.bindings());
async function reloadBindings() {
  try {
    await bindings.reload();
  } catch (error) {
    errorToast(error);
  }
}

const columns = [
  { dataIndex: 'id', title: 'ID', width: 80 },
  { key: 'code', title: '编号 / 版本' },
  { key: 'status', title: '状态', width: 150 },
  { key: 'summary', title: '关键配置' },
  { key: 'effective', title: '生效时间', width: 130 },
  { key: 'created', title: '创建', width: 130 },
];

const notYetEffective = (v: PolicyVersion) =>
  v.status === 'active' && (!v.effective_at || Date.parse(v.effective_at) > Date.now());

/* 详情 */
const selected = ref<null | { id: string; kind: PolicyKind }>(null);
const detail = useResource(() => (selected.value ? policiesApi.get(selected.value.kind, selected.value.id) : null));
const drawerOpen = computed({
  get: () => !!selected.value,
  set: (v) => {
    if (!v) selected.value = null;
  },
});
const current = computed(() => (selected.value ? detail.data.value : null));
const detailKind = computed(() => selected.value?.kind ?? kind.value);
const isBound = computed(() => !!current.value && bindings.data.value?.bindings[current.value.kind] === current.value.id);
const extraConfig = computed(() => {
  const c = current.value;
  if (!c) return [];
  const known = new Set(POLICY_FIELDS[c.kind].map((f) => f.key));
  return Object.entries(c.config).filter(([k]) => !known.has(k));
});

async function reloadDetail() {
  try {
    await detail.reload();
  } catch (error) {
    errorToast(error);
  }
}

async function openVersion(v: PolicyVersion) {
  selected.value = { id: v.id, kind: v.kind };
  await reloadDetail();
}

async function afterWrite() {
  await Promise.all([reloadDetail(), list.load(), loadVersions(detailKind.value)]);
}

/* GPU 型号候选：节点上已发现的 GPU 型号，首次打开 GPU 定价编辑时加载。失败时仍可手动输入。 */
const gpuModels = ref<string[]>([]);
let gpuModelsLoaded = false;
async function loadGpuModels() {
  if (gpuModelsLoaded) return;
  gpuModelsLoaded = true;
  try {
    const nodes = await listAll(nodesApi.list);
    const gpus = await Promise.all(nodes.map((n) => listAll((q) => nodesApi.gpus(n.id, q))));
    gpuModels.value = [...new Set(gpus.flat().map((g) => g.model).filter(Boolean))].toSorted();
  } catch (error) {
    gpuModelsLoaded = false;
    errorToast(error);
  }
}

/* 新建 / 编辑草稿 */
const editorOpen = ref(false);
const editorDraft = ref<null | PolicyVersion>(null);
const editorKind = ref<PolicyKind>('pricing');
function openEditor(draft: null | PolicyVersion) {
  editorDraft.value = draft;
  editorKind.value = draft?.kind ?? kind.value;
  if (editorKind.value === 'gpu-pricing') void loadGpuModels();
  editorOpen.value = true;
}
async function onSaved(v: PolicyVersion) {
  if (editorDraft.value) {
    await afterWrite();
    return;
  }
  void list.load();
  void loadVersions(v.kind);
  await openVersion(v);
}

/* 发布 */
const publishOpen = ref(false);

/* 退役 */
const retireOpen = ref(false);
const retireAction = useAction();
async function retire() {
  const c = current.value;
  if (!c) return false;
  const r = await retireAction.run(
    (key) => policiesApi.retire(c.kind, c.id, { ifMatch: detail.etag.value, key }),
    { onConflict: reloadDetail, success: '已退役' },
  );
  if (!r) return false;
  await afterWrite();
  return true;
}

onMounted(() => {
  void list.load();
  void reloadBindings();
  for (const k of POLICY_KINDS) void loadVersions(k);
});
</script>

<template>
  <Page title="政策与定价">
    <BindingsCard
      class="mb-3"
      :bindings="bindings.data.value"
      :etag="bindings.etag.value"
      :loading="bindings.loading.value"
      :versions="allVersions"
      @conflict="reloadBindings"
      @saved="reloadBindings"
    />

    <Card size="small">
      <div class="flex gap-3">
        <div class="w-36 shrink-0">
          <Menu v-model:selected-keys="menuKeys" class="!border-e-0" mode="inline">
            <MenuItem v-for="k in POLICY_KINDS" :key="k">{{ POLICY_KIND_NAMES[k] }}</MenuItem>
          </Menu>
        </div>
        <div class="min-w-0 flex-1">
          <div class="mb-3 flex items-center justify-between">
            <span class="font-medium">{{ POLICY_KIND_NAMES[kind] }} <span class="text-muted-foreground font-mono text-xs">{{ kind }}</span></span>
            <Button type="primary" @click="openEditor(null)">新建草稿</Button>
          </div>
          <Table
            :columns="columns"
            :custom-row="(r: PolicyVersion) => ({ onClick: () => openVersion(r), style: 'cursor:pointer' })"
            :data-source="list.items.value"
            :loading="list.loading.value"
            :pagination="false"
            row-key="id"
            :scroll="{ x: 900 }"
            size="small"
          >
            <template #bodyCell="{ column, record }">
              <span v-if="column.dataIndex === 'id'" class="font-mono">{{ record.id }}</span>
              <span v-else-if="column.key === 'code'" class="font-mono">{{ versionLabel(record as PolicyVersion) }}</span>
              <Space v-else-if="column.key === 'status'" :size="4" wrap>
                <StatusTag kind="catalogStatus" :value="record.status" />
                <Tag v-if="notYetEffective(record as PolicyVersion)" :bordered="false" color="processing">待生效</Tag>
                <Tag v-if="bindings.data.value?.bindings[kind] === record.id" :bordered="false" color="blue">平台默认</Tag>
              </Space>
              <span v-else-if="column.key === 'summary'" class="text-xs">{{ configSummary(record.kind, record.config) }}</span>
              <TimeText v-else-if="column.key === 'effective'" :value="record.status === 'draft' ? null : record.effective_at" />
              <TimeText v-else-if="column.key === 'created'" :value="record.created_at" />
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
      </div>
    </Card>

    <Drawer v-model:open="drawerOpen" :title="current ? versionLabel(current) : '政策版本'" :width="600">
      <template v-if="current" #extra>
        <Space>
          <template v-if="current.status === 'draft'">
            <Button @click="openEditor(current)">编辑</Button>
            <Button type="primary" @click="publishOpen = true">发布</Button>
          </template>
          <Tooltip v-else-if="current.status === 'active'" :title="isBound ? '该版本是平台默认绑定，请先在平台默认绑定中替换' : ''">
            <Button danger :disabled="isBound" @click="retireOpen = true">退役</Button>
          </Tooltip>
        </Space>
      </template>
      <template v-if="current">
        <Descriptions bordered :column="1" size="small">
          <DescriptionsItem label="ID"><span class="font-mono">{{ current.id }}</span></DescriptionsItem>
          <DescriptionsItem label="类别">{{ POLICY_KIND_NAMES[current.kind] }}</DescriptionsItem>
          <DescriptionsItem label="编号 / 版本"><span class="font-mono">{{ current.code }} v{{ current.version }}</span></DescriptionsItem>
          <DescriptionsItem label="状态">
            <Space :size="4">
              <StatusTag kind="catalogStatus" :value="current.status" />
              <Tag v-if="notYetEffective(current)" :bordered="false" color="processing">待生效</Tag>
              <Tag v-if="isBound" :bordered="false" color="blue">平台默认</Tag>
            </Space>
          </DescriptionsItem>
          <DescriptionsItem label="生效时间">
            <TimeText :value="current.status === 'draft' ? null : current.effective_at" />
          </DescriptionsItem>
          <DescriptionsItem label="创建"><TimeText :value="current.created_at" /></DescriptionsItem>
          <DescriptionsItem label="更新"><TimeText :value="current.updated_at" /></DescriptionsItem>
          <DescriptionsItem label="创建者"><UserName :id="current.created_by_user_id ?? null" /></DescriptionsItem>
        </Descriptions>
        <h4 class="mb-2 mt-4 font-medium">配置</h4>
        <Descriptions bordered :column="1" size="small">
          <DescriptionsItem v-for="f in POLICY_FIELDS[current.kind]" :key="f.key">
            <template #label>
              {{ f.label }}
              <div class="text-muted-foreground font-mono text-xs">{{ f.key }}</div>
            </template>
            <span class="font-mono">{{ displayValue(f, current.config[f.key]) }}</span>
          </DescriptionsItem>
          <DescriptionsItem v-for="[k, v] in extraConfig" :key="k" :label="k">
            <span class="font-mono">{{ rawValue(v) }}</span>
          </DescriptionsItem>
        </Descriptions>
      </template>
    </Drawer>

    <VersionEditor
      v-model:open="editorOpen"
      :draft="editorDraft"
      :gpu-models="gpuModels"
      :if-match="detail.etag.value"
      :kind="editorKind"
      @conflict="reloadDetail"
      @saved="onSaved"
    />

    <PublishModal
      v-if="current && current.status === 'draft'"
      v-model:open="publishOpen"
      :bound-id="bindings.data.value?.bindings[current.kind]"
      :draft="current"
      :if-match="detail.etag.value"
      :kind="current.kind"
      :versions="allVersions[current.kind] ?? []"
      @conflict="reloadDetail"
      @published="afterWrite"
    />

    <ConfirmAction
      v-model:open="retireOpen"
      danger
      :description="current ? `退役 ${versionLabel(current)}。退役后不能再被平台默认绑定选用；已开始的计费区间和停机期限不受影响。` : ''"
      :on-confirm="retire"
      title="退役政策版本"
    />
  </Page>
</template>
