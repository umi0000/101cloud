<script lang="ts" setup>
import type { Zone } from '#/api/types';

import { onMounted, ref } from 'vue';

import { Button, Form, FormItem, Input, message, Modal, Select, Space, Table } from 'ant-design-vue';

import { catalogApi } from '#/api/backend';
import { revisionTag } from '#/api/request';
import CursorPager from '#/components/cursor-pager.vue';
import StatusTag from '#/components/status-tag.vue';
import TimeText from '#/components/time-text.vue';
import { useAction, useCursorList } from '#/composables/api';

import { CODE_PATTERN } from './rules';

const emit = defineEmits<{ changed: [] }>();

const list = useCursorList<Zone>(catalogApi.zones, ref({}));
const action = useAction();

const columns = [
  { dataIndex: 'id', title: 'ID', width: 70 },
  { dataIndex: 'code', title: '代码' },
  { dataIndex: 'name', title: '名称' },
  { key: 'status', title: '状态', width: 90 },
  { key: 'updated', title: '更新', width: 130 },
  { key: 'actions', title: '', width: 80 },
];

const createOpen = ref(false);
const createForm = ref({ code: '', name: '' });

function openCreate() {
  createForm.value = { code: '', name: '' };
  action.resetKey();
  createOpen.value = true;
}

async function submitCreate() {
  const { code, name } = createForm.value;
  if (!CODE_PATTERN.test(code)) return message.warning('代码须为字母或数字开头，1–64 位字母、数字、_ . -');
  if (!name.trim() || name.length > 128) return message.warning('名称为 1–128 个字符');
  const r = await action.run((key) => catalogApi.createZone({ code, name }, { key }), { success: '已创建可用区' });
  if (!r) return;
  createOpen.value = false;
  await list.load();
  emit('changed');
}

const editing = ref<null | Zone>(null);
const editForm = ref<{ name: string; status: Zone['status'] }>({ name: '', status: 'active' });

function openEdit(z: Zone) {
  editing.value = z;
  editForm.value = { name: z.name, status: z.status };
}

async function submitEdit() {
  const z = editing.value;
  if (!z) return;
  const { name, status } = editForm.value;
  if (!name.trim() || name.length > 128) return message.warning('名称为 1–128 个字符');
  const body: { name?: string; status?: Zone['status'] } = {};
  if (name !== z.name) body.name = name;
  if (status !== z.status) body.status = status;
  if (Object.keys(body).length === 0) {
    editing.value = null;
    return;
  }
  const r = await action.run((key) => catalogApi.updateZone(z.id, body, { ifMatch: revisionTag(z.revision), key }), {
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
    <Space class="mb-3">
      <Button type="primary" @click="openCreate">新建可用区</Button>
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
        <template v-if="column.dataIndex === 'id' || column.dataIndex === 'code'">
          <span class="font-mono">{{ record[column.dataIndex] }}</span>
        </template>
        <template v-else-if="column.key === 'status'"><StatusTag kind="simpleStatus" :value="record.status" /></template>
        <template v-else-if="column.key === 'updated'"><TimeText :value="record.updated_at" /></template>
        <template v-else-if="column.key === 'actions'">
          <Button size="small" type="link" @click="openEdit(record as Zone)">编辑</Button>
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

    <Modal v-model:open="createOpen" :confirm-loading="action.pending.value" title="新建可用区" @ok="submitCreate">
      <Form layout="vertical">
        <FormItem label="代码" required extra="创建后不可修改">
          <Input v-model:value="createForm.code" class="font-mono" :maxlength="64" @change="action.resetKey()" />
        </FormItem>
        <FormItem label="名称" required>
          <Input v-model:value="createForm.name" :maxlength="128" @change="action.resetKey()" />
        </FormItem>
      </Form>
    </Modal>

    <Modal
      :confirm-loading="action.pending.value"
      :open="!!editing"
      :title="`编辑可用区 ${editing?.code ?? ''}`"
      @cancel="editing = null"
      @ok="submitEdit"
    >
      <Form layout="vertical">
        <FormItem label="名称" required>
          <Input v-model:value="editForm.name" :maxlength="128" />
        </FormItem>
        <FormItem label="状态" extra="停用后不能在该可用区创建实例，也不能发布引用它的套餐">
          <Select
            v-model:value="editForm.status"
            :options="[
              { label: '启用', value: 'active' },
              { label: '停用', value: 'disabled' },
            ]"
          />
        </FormItem>
      </Form>
    </Modal>
  </div>
</template>
