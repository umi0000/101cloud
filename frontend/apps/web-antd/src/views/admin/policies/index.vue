<script lang="ts" setup>
/**
 * 政策与定价（前端设计 7.8）：左侧 6 类政策，右侧配置档列表。
 * 点击配置档打开抽屉直接编辑：保存（PATCH）、删除（DELETE）；右上角新建配置档。
 */
import type { FormState } from './components/fields';

import type { PolicyKind, PolicyVersion } from '#/api/types';

import { computed, onMounted, ref, watch } from 'vue';

import { Page } from '@vben/common-ui';

import { Alert, Button, Card, Descriptions, DescriptionsItem, Drawer, Form, Menu, MenuItem, Space, Table } from 'ant-design-vue';

import { listAll, nodesApi, policiesApi } from '#/api/backend';
import { POLICY_KINDS } from '#/api/types';
import ConfirmAction from '#/components/confirm-action.vue';
import CursorPager from '#/components/cursor-pager.vue';
import StatusTag from '#/components/status-tag.vue';
import TimeText from '#/components/time-text.vue';
import UserName from '#/components/user-name.vue';
import { errorToast, useAction, useCursorList, useResource } from '#/composables/api';
import { POLICY_KIND_NAMES } from '#/locales/enums';

import ConfigForm from './components/config-form.vue';
import { configFromState, configSummary, emptyState, stateFromConfig, validateState, versionLabel } from './components/fields';
import VersionEditor from './components/version-editor.vue';

const kind = ref<PolicyKind>('pricing');
const menuKeys = computed({
  get: () => [kind.value],
  set: (keys: (number | string)[]) => {
    const k = POLICY_KINDS.find((x) => x === keys[0]);
    if (k) kind.value = k;
  },
});

/* 配置档列表（游标分页）。后端列表只接受 limit/cursor。 */
const list = useCursorList<PolicyVersion>((q) => policiesApi.list(kind.value, q), ref({}));
watch(kind, () => list.reset());



const columns = [
  { dataIndex: 'id', title: 'ID', width: 80 },
  { key: 'code', title: '编号 / 版本' },
  { key: 'status', title: '状态', width: 120 },
  { key: 'summary', title: '关键配置' },
  { key: 'created', title: '创建', width: 130 },
];

/* 详情抽屉：直接编辑 config */
const selected = ref<null | { id: string; kind: PolicyKind }>(null);
const detail = useResource(() => (selected.value ? policiesApi.get(selected.value.kind, selected.value.id) : null));
const drawerOpen = computed({
  get: () => !!selected.value,
  set: (v) => {
    if (!v) selected.value = null;
  },
});
const current = computed(() => {
  const s = selected.value;
  const d = detail.data.value;
  return s && d && d.id === s.id && d.kind === s.kind ? d : null;
});

const editState = ref<FormState>(emptyState('pricing'));
const editSubmitted = ref(false);
watch(current, (c) => {
  if (!c) return;
  editState.value = stateFromConfig(c.kind, c.config);
  editSubmitted.value = false;
});
const editValidation = computed<ReturnType<typeof validateState>>(() =>
  current.value ? validateState(current.value.kind, editState.value) : { cross: [], fields: {} },
);
const editErrors = computed<Record<string, string>>(() => (editSubmitted.value ? editValidation.value.fields : {}));

async function reloadDetail() {
  try {
    await detail.reload();
  } catch (error) {
    errorToast(error);
  }
}

async function openVersion(v: PolicyVersion) {
  if (v.kind === 'gpu-pricing') void loadGpuModels();
  selected.value = { id: v.id, kind: v.kind };
  await reloadDetail();
}

async function afterWrite() {
  await list.load();
}

const saveAction = useAction();
async function save() {
  const c = current.value;
  if (!c) return;
  editSubmitted.value = true;
  const v = editValidation.value;
  if (Object.keys(v.fields).length > 0 || v.cross.length > 0) return;
  const config = configFromState(c.kind, editState.value);
  const r = await saveAction.run(() => policiesApi.update(c.kind, c.id, config, { ifMatch: detail.etag.value }), {
    onConflict: reloadDetail,
    success: '已保存',
  });
  if (!r) return;
  await Promise.all([reloadDetail(), afterWrite()]);
}


/* 删除：仍被引用时后端返回 409，提示后端 message。 */
const removeOpen = ref(false);
const removeAction = useAction();
async function remove() {
  const c = current.value;
  if (!c) return false;
  const r = await removeAction.run((key) => policiesApi.remove(c.kind, c.id, { ifMatch: detail.etag.value, key }), {
    onConflict: reloadDetail,
    success: '已删除',
  });
  if (!r) return false;
  selected.value = null;
  await afterWrite();
  return true;
}

/* GPU 型号候选：节点上已发现的 GPU 型号，首次新建或打开 GPU 定价时加载。失败时仍可手动输入。 */
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

/* 新建配置档 */
const editorOpen = ref(false);
const editorKind = ref<PolicyKind>('pricing');
function openEditor() {
  editorKind.value = kind.value;
  if (editorKind.value === 'gpu-pricing') void loadGpuModels();
  editorOpen.value = true;
}
async function onCreated(v: PolicyVersion) {
  void afterWrite();
  await openVersion(v);
}

onMounted(() => {
  void list.load();
});
</script>

<template>
  <Page title="政策与定价">
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
            <Button type="primary" @click="openEditor">新建配置档</Button>
          </div>
          <Table
            :columns="columns"
            :custom-row="(r: PolicyVersion) => ({ onClick: () => openVersion(r), style: 'cursor:pointer' })"
            :data-source="list.items.value"
            :loading="list.loading.value"
            :pagination="false"
            row-key="id"
            :scroll="{ x: 800 }"
            size="small"
          >
            <template #bodyCell="{ column, record }">
              <span v-if="column.dataIndex === 'id'" class="font-mono">{{ record.id }}</span>
              <span v-else-if="column.key === 'code'" class="font-mono">{{ versionLabel(record as PolicyVersion) }}</span>
              <Space v-else-if="column.key === 'status'" :size="4" wrap>
                <StatusTag v-if="record.status === 'retired'" kind="catalogStatus" value="retired" />
              </Space>
              <span v-else-if="column.key === 'summary'" class="text-xs">{{ configSummary(record.kind, record.config) }}</span>
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

    <Drawer v-model:open="drawerOpen" :title="current ? versionLabel(current) : '配置档'" :width="640">
      <template v-if="current" #extra>
        <Space>
          <Button danger @click="removeOpen = true">删除</Button>
          <Button :loading="saveAction.pending.value" type="primary" @click="save">保存</Button>
        </Space>
      </template>
      <template v-if="current">
        <Descriptions bordered :column="2" size="small">
          <DescriptionsItem label="ID"><span class="font-mono">{{ current.id }}</span></DescriptionsItem>
          <DescriptionsItem label="编号 / 版本"><span class="font-mono">{{ current.code }} v{{ current.version }}</span></DescriptionsItem>
          <DescriptionsItem label="创建"><TimeText :value="current.created_at" /></DescriptionsItem>
          <DescriptionsItem label="更新"><TimeText :value="current.updated_at" /></DescriptionsItem>
          <DescriptionsItem label="创建者"><UserName :id="current.created_by_user_id ?? null" /></DescriptionsItem>
          <DescriptionsItem v-if="current.status === 'retired'" label="状态">
            <StatusTag kind="catalogStatus" value="retired" />
          </DescriptionsItem>
        </Descriptions>
        <Form class="mt-4" layout="vertical">
          <ConfigForm v-model:state="editState" :errors="editErrors" :gpu-models="gpuModels" :kind="current.kind" />
          <Alert
            v-if="editSubmitted && editValidation.cross.length > 0"
            class="mb-2"
            :message="editValidation.cross.join('；')"
            show-icon
            type="error"
          />
        </Form>
      </template>
    </Drawer>

    <VersionEditor v-model:open="editorOpen" :gpu-models="gpuModels" :kind="editorKind" @created="onCreated" />


    <ConfirmAction
      v-model:open="removeOpen"
      danger
      :description="current ? `删除 ${versionLabel(current)}。仍被套餐、实例、额度账户、节点或使用记录引用的配置档不能删除。` : ''"
      ok-text="删除"
      :on-confirm="remove"
      title="删除配置档"
    />
  </Page>
</template>
