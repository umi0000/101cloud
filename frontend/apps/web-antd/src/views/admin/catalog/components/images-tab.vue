<script lang="ts" setup>
import type { Image, InstanceType } from '#/api/types';

import { onMounted, ref } from 'vue';

import {
  Alert,
  Button,
  Form,
  FormItem,
  Input,
  message,
  Modal,
  Select,
  Space,
  Switch,
  Table,
  Tag,
  Textarea,
  Tooltip,
} from 'ant-design-vue';

import { catalogApi } from '#/api/backend';
import { revisionTag } from '#/api/request';
import CursorPager from '#/components/cursor-pager.vue';
import StatusTag from '#/components/status-tag.vue';
import { useAction, useCursorList } from '#/composables/api';
import { ENUMS } from '#/locales/enums';
import { shortId } from '#/utils/format';

import { CODE_PATTERN, jsonForbidden, SHA256_PATTERN } from './rules';

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

const columns = [
  { dataIndex: 'id', title: 'ID', width: 70 },
  { dataIndex: 'name', title: '名称' },
  { key: 'type', title: '类型', width: 80 },
  { dataIndex: 'architecture', title: '架构', width: 90 },
  { key: 'gpu', title: 'GPU 就绪', width: 90 },
  { key: 'os', title: '系统' },
  { key: 'fingerprint', title: '指纹', width: 130 },
  { key: 'status', title: '状态', width: 80 },
  { key: 'actions', title: '', width: 70 },
];

interface CreateForm {
  architecture: string;
  code: string;
  fingerprint: string;
  gpu_ready: boolean;
  instance_type: InstanceType;
  os_name: string;
  os_release: string;
  version: string;
}
const emptyCreate = (): CreateForm => ({
  architecture: 'x86_64',
  code: '',
  fingerprint: '',
  gpu_ready: false,
  instance_type: 'container',
  os_name: '',
  os_release: '',
  version: '',
});
const createOpen = ref(false);
const createForm = ref<CreateForm>(emptyCreate());

function openCreate() {
  createForm.value = emptyCreate();
  action.resetKey();
  createOpen.value = true;
}

function textOk(s: string, max: number) {
  return s.trim().length > 0 && [...s].length <= max;
}

async function submitCreate() {
  const f = createForm.value;
  if ((f.code && !CODE_PATTERN.test(f.code)) || (f.version && !CODE_PATTERN.test(f.version))) {
    return message.warning('代码与版本可留空；填写时须为字母或数字开头，1–64 位字母、数字、_ . -');
  }
  if (!textOk(f.architecture, 32)) return message.warning('架构为 1–32 个字符');
  const fingerprint = f.fingerprint.trim().toLowerCase();
  if (/^[0-9a-f]{12}$/.test(fingerprint)) {
    return message.warning(
      '这是 incus image list 默认显示的 12 位短指纹；请用 incus image list -c lFd 或 incus image info <短指纹> 查看 64 位完整指纹',
    );
  }
  if (!SHA256_PATTERN.test(fingerprint)) return message.warning('指纹须为 64 位十六进制 SHA-256 完整指纹');
  if (!textOk(f.os_name, 64) || !textOk(f.os_release, 64)) return message.warning('系统名称与版本为 1–64 个字符');
  const properties = { gpu_ready: f.gpu_ready, os_name: f.os_name, os_release: f.os_release };
  const bad = jsonForbidden(properties);
  if (bad.length > 0) return message.warning(`properties 不能包含：${bad.join('、')}`);
  const body = {
    architecture: f.architecture,
    code: f.code,
    fingerprint,
    instance_type: f.instance_type,
    properties,
    version: f.version,
  };
  const r = await action.run((key) => catalogApi.createImage(body, { key }), { success: '已登记镜像' });
  if (!r) return;
  createOpen.value = false;
  await list.load();
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
  try {
    parsed = JSON.parse(editProps.value);
  } catch {
    return message.warning('properties 不是合法 JSON');
  }
  if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
    return message.warning('properties 必须是 JSON 对象');
  }
  const bad = jsonForbidden(parsed);
  if (bad.length > 0) return message.warning(`properties 不能包含：${bad.join('、')}`);
  const body: { properties?: Image['properties']; status?: Image['status'] } = {};
  if (JSON.stringify(parsed) !== JSON.stringify(img.properties)) body.properties = parsed as Image['properties'];
  if (editStatus.value !== img.status) body.status = editStatus.value;
  if (Object.keys(body).length === 0) {
    editing.value = null;
    return;
  }
  const r = await action.run((key) => catalogApi.updateImage(img.id, body, { ifMatch: revisionTag(img.revision), key }), {
    onConflict: async () => {
      editing.value = null;
      await list.load();
    },
    success: '已保存',
  });
  if (!r) return;
  editing.value = null;
  await list.load();
  emit('changed');
}

onMounted(() => list.load());
</script>

<template>
  <div>
    <Space class="mb-3" wrap>
      <Button type="primary" @click="openCreate">登记镜像</Button>
      <Select v-model:value="filters.instance_type" allow-clear class="w-28" :options="typeOptions" placeholder="类型" />
      <Input v-model:value="filters.architecture" allow-clear class="w-32" placeholder="架构，如 x86_64" />
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
        <template v-if="column.dataIndex === 'id' || column.dataIndex === 'architecture'">
          <span class="font-mono">{{ record[column.dataIndex] }}</span>
        </template>
        <template v-else-if="column.key === 'type'"><StatusTag kind="instanceType" :value="record.instance_type" /></template>
        <template v-else-if="column.key === 'gpu'">
          <Tag v-if="record.gpu_ready" color="success">是</Tag>
          <span v-else class="text-muted-foreground">否</span>
        </template>
        <template v-else-if="column.key === 'os'">{{ record.os_name }} {{ record.os_release }}</template>
        <template v-else-if="column.key === 'fingerprint'">
          <Tooltip :title="record.fingerprint">
            <span class="font-mono text-xs">{{ shortId(record.fingerprint) }}</span>
          </Tooltip>
        </template>
        <template v-else-if="column.key === 'status'">
          <StatusTag :label="IMAGE_STATUS[record.status as Image['status']]" />
        </template>
        <template v-else-if="column.key === 'actions'">
          <Button size="small" type="link" @click="openEdit(record as Image)">编辑</Button>
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

    <Modal
      v-model:open="createOpen"
      :confirm-loading="action.pending.value"
      title="登记镜像"
      :width="560"
      @ok="submitCreate"
    >
      <Alert
        class="mb-3"
        message="只登记节点上已有的本地镜像，不会下载或上传。代码和版本可留空：留空时代码取指纹前 12 位、版本为 1，名称为“系统名称 系统版本（代码）”；填写时名称为“代码 版本”。登记后只能修改 properties 和状态。"
        show-icon
        type="info"
      />
      <Form layout="vertical" @change="action.resetKey()">
        <div class="grid grid-cols-2 gap-x-3">
          <FormItem label="代码"><Input v-model:value="createForm.code" class="font-mono" placeholder="留空则取指纹前 12 位" /></FormItem>
          <FormItem label="版本"><Input v-model:value="createForm.version" class="font-mono" placeholder="留空则为 1" /></FormItem>
          <FormItem label="类型" required>
            <Select v-model:value="createForm.instance_type" :options="typeOptions" />
          </FormItem>
          <FormItem label="架构" required><Input v-model:value="createForm.architecture" class="font-mono" /></FormItem>
        </div>
        <FormItem
          label="指纹（SHA-256）"
          required
          extra="64 位完整指纹。incus image list 默认只显示 12 位短指纹，请用 incus image list -c lFd 或 incus image info <短指纹> 查看完整值"
        >
          <Input v-model:value="createForm.fingerprint" class="font-mono" :maxlength="80" />
        </FormItem>
        <div class="grid grid-cols-2 gap-x-3">
          <FormItem label="系统名称" required><Input v-model:value="createForm.os_name" placeholder="ubuntu" /></FormItem>
          <FormItem label="系统版本" required><Input v-model:value="createForm.os_release" placeholder="24.04" /></FormItem>
        </div>
        <FormItem label="GPU 就绪" extra="镜像已安装 GPU 驱动/用户态库；带 GPU 的套餐和实例只能使用 GPU 就绪镜像">
          <Switch v-model:checked="createForm.gpu_ready" />
        </FormItem>
      </Form>
    </Modal>

    <Modal
      :confirm-loading="action.pending.value"
      :open="!!editing"
      :title="`编辑镜像 ${editing?.name ?? ''}`"
      :width="560"
      @cancel="editing = null"
      @ok="submitEdit"
    >
      <Form layout="vertical">
        <FormItem label="状态">
          <Select
            v-model:value="editStatus"
            :options="[
              { label: '可用', value: 'active' },
              { label: '停用', value: 'disabled' },
              { label: '已退役', value: 'retired' },
            ]"
          />
        </FormItem>
        <FormItem
          label="properties（JSON 对象）"
          extra="后端只保存这段 JSON；列表中的 GPU 就绪、系统名称与版本以登记时为准，不随此处修改。不能包含密码、密钥、令牌或 cloud-init 内容。"
        >
          <Textarea v-model:value="editProps" :auto-size="{ minRows: 4, maxRows: 14 }" class="font-mono text-xs" />
        </FormItem>
      </Form>
    </Modal>
  </div>
</template>
