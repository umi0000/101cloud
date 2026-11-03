<script lang="ts" setup>
import type { Image, InstanceType, Node } from '#/api/types';

import { onMounted, ref } from 'vue';

import { Button, Form, FormItem, Input, message, Modal, Select, Space, Table, Textarea } from 'ant-design-vue';

import { catalogApi, listAll, nodesApi } from '#/api/backend';
import { revisionTag } from '#/api/request';
import CursorPager from '#/components/cursor-pager.vue';
import StatusTag from '#/components/status-tag.vue';
import { errorToast, useAction, useCursorList, usePolling } from '#/composables/api';
import { ENUMS } from '#/locales/enums';

import ImageNodeStatus from './image-node-status.vue';
import ImageRegisterDrawer from './image-register-drawer.vue';
import { jsonForbidden } from './rules';

const emit = defineEmits<{ changed: [] }>();
const IMAGE_STATUS = {
  active: { color: 'success', text: '可用' },
  disabled: { color: 'default', text: '停用' },
  retired: { color: 'default', text: '已退役' },
} as const;
const typeOptions = Object.entries(ENUMS.instanceType ?? {}).map(([value, l]) => ({ label: l.text, value }));
const filters = ref<{ architecture?: string; instance_type?: InstanceType }>({});
const list = useCursorList<Image, typeof filters.value>(catalogApi.images, filters);
const action = useAction();
const nodes = ref<Node[]>([]);
const columns = [
  { dataIndex: 'id', title: 'ID', width: 70 },
  { dataIndex: 'name', title: '名称' },
  { key: 'type', title: '类型', width: 80 },
  { key: 'os', title: '系统' },
  { key: 'nodes', title: '节点配置 / 下载状态' },
  { key: 'status', title: '状态', width: 80 },
  { key: 'actions', title: '', width: 130 },
];
const createOpen = ref(false);
const managing = ref<Image | null>(null);
function openCreate() {
  managing.value = null;
  createOpen.value = true;
}
function manage(image: Image) {
  managing.value = image;
  createOpen.value = true;
}
async function changed() {
  await list.load();
  if (managing.value) managing.value = list.items.value.find((i) => i.id === managing.value?.id) ?? managing.value;
  emit('changed');
}
const editing = ref<Image | null>(null);
const editStatus = ref<Image['status']>('active');
const editProps = ref('');
function openEdit(img: Image) {
  editing.value = img;
  editStatus.value = img.status;
  editProps.value = JSON.stringify(img.properties, null, 2);
}
async function submitEdit() {
  const img = editing.value;
  if (!img) return;
  let parsed: unknown;
  try { parsed = JSON.parse(editProps.value); } catch { return message.warning('properties 不是合法 JSON'); }
  if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) return message.warning('properties 必须是 JSON 对象');
  const bad = jsonForbidden(parsed);
  if (bad.length > 0) return message.warning(`properties 不能包含：${bad.join('、')}`);
  const body: { properties?: Image['properties']; status?: Image['status'] } = {};
  if (JSON.stringify(parsed) !== JSON.stringify(img.properties)) body.properties = parsed as Image['properties'];
  if (editStatus.value !== img.status) body.status = editStatus.value;
  if (Object.keys(body).length === 0) { editing.value = null; return; }
  const response = await action.run((key) => catalogApi.updateImage(img.id, body, { ifMatch: revisionTag(img.revision), key }), {
    onConflict: async () => { editing.value = null; await list.load(); },
    success: '已保存',
  });
  if (!response) return;
  editing.value = null;
  await changed();
}
onMounted(async () => {
  await list.load();
  try { nodes.value = await listAll(nodesApi.list); } catch (error) { errorToast(error); }
});
usePolling(async () => {
  if (list.items.value.some((i) => i.nodes.some((n) => n.status === 'pending' || n.status === 'downloading' || n.status === 'unknown'))) await list.load();
}, () => 4000);
</script>

<template>
  <div>
    <div class="mb-3 flex flex-wrap items-center gap-2">
      <Button type="primary" @click="openCreate">登记镜像</Button>
      <Select v-model:value="filters.instance_type" allow-clear class="!w-28" :options="typeOptions" placeholder="类型" />
      <Input v-model:value="filters.architecture" allow-clear class="!w-40" placeholder="节点架构，如 x86_64" />
    </div>
    <Table :columns="columns" :data-source="list.items.value" :loading="list.loading.value" :pagination="false" row-key="id" size="small" :scroll="{ x: 1000 }">
      <template #bodyCell="{ column, record }">
        <template v-if="column.dataIndex === 'id'"><span class="font-mono">{{ record.id }}</span></template>
        <template v-else-if="column.key === 'type'"><StatusTag kind="instanceType" :value="record.instance_type" /></template>
        <template v-else-if="column.key === 'os'">{{ record.os_name }} {{ record.os_release }}</template>
        <template v-else-if="column.key === 'nodes'"><ImageNodeStatus :image="record as Image" :nodes="nodes" @changed="changed" /></template>
        <template v-else-if="column.key === 'status'"><StatusTag :label="IMAGE_STATUS[record.status as Image['status']]" /></template>
        <template v-else-if="column.key === 'actions'"><Space direction="vertical" :size="0"><Button size="small" type="link" @click="manage(record as Image)">配置节点</Button><Button size="small" type="link" @click="openEdit(record as Image)">编辑属性</Button></Space></template>
      </template>
    </Table>
    <CursorPager :has-next="list.hasNext()" :has-prev="list.hasPrev()" :loading="list.loading.value" :page="list.page()" @next="list.nextPage" @prev="list.prevPage" @refresh="list.load" />
    <ImageRegisterDrawer v-model:open="createOpen" :image="managing" @changed="changed" />
    <Modal :confirm-loading="action.pending.value" :open="!!editing" :title="`编辑镜像 ${editing?.name ?? ''}`" :width="560" @cancel="editing = null" @ok="submitEdit">
      <Form layout="vertical">
        <FormItem label="状态"><Select v-model:value="editStatus" :options="[{ label: '可用', value: 'active' }, { label: '停用', value: 'disabled' }, { label: '已退役', value: 'retired' }]" /></FormItem>
        <FormItem label="properties（JSON 对象）" extra="此处保存逻辑镜像属性。节点架构、指纹、cloud-init 和 GPU 就绪请在“配置节点”中管理；不能包含密码、密钥、令牌或 cloud-init 内容。"><Textarea v-model:value="editProps" :auto-size="{ minRows: 4, maxRows: 14 }" class="font-mono text-xs" /></FormItem>
      </Form>
    </Modal>
  </div>
</template>
