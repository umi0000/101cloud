<script lang="ts" setup>
import type { FormInstance } from 'ant-design-vue';

import type { Node, NodeConnectionInput, Zone } from '#/api/types';

import type { AllocatableForm } from './node-forms';

import { ref, watch } from 'vue';

import { Alert, Button, Col, Divider, Drawer, Form, FormItem, Input, Row, Select } from 'ant-design-vue';

import { nodesApi } from '#/api/backend';
import { useAction } from '#/composables/api';

import AllocatableFields from './allocatable-fields.vue';
import ConnectionFields from './connection-fields.vue';
import { allocatableFromForm, connectionBody, emptyAllocatable, emptyConnection } from './node-forms';

const props = defineProps<{ zones: Zone[] }>();
const emit = defineEmits<{ created: [node: Node] }>();
const open = defineModel<boolean>('open', { default: false });

interface RegisterForm {
  allocatable: AllocatableForm;
  connection: NodeConnectionInput;
  defaults: { project_name: string; storage_pool: string };
  name: string;
  zone_id: string | undefined;
}

const blank = (): RegisterForm => ({
  allocatable: emptyAllocatable(),
  connection: emptyConnection(),
  defaults: { project_name: 'default', storage_pool: 'default' },
  name: '',
  zone_id: undefined,
});

const form = ref<RegisterForm>(blank());
const formRef = ref<FormInstance>();
const action = useAction();

watch(open, (v) => {
  if (v) form.value = blank();
});
watch(form, () => action.resetKey(), { deep: true });

const nameRules = [
  { message: '请填写名称', required: true, whitespace: true },
  { max: 128, message: '最多 128 字符' },
];
const projectRules = [
  { message: '请填写 Incus 项目', required: true },
  {
    message: '字母或数字开头，只含字母、数字、_ . -，最多 63 字符',
    pattern: /^[A-Za-z\d][\w.-]{0,62}$/,
  },
];
const poolRules = [
  { message: '请填写存储池', required: true, whitespace: true },
  { max: 255, message: '最多 255 字符' },
];

async function submit() {
  try {
    await formRef.value?.validate();
  } catch {
    return;
  }
  const f = form.value;
  if (!f.zone_id) return;
  const zoneId = f.zone_id;
  const r = await action.run(
    (key) =>
      nodesApi.create(
        {
          allocatable: allocatableFromForm(f.allocatable),
          connection: connectionBody(f.connection),
          defaults: { project_name: f.defaults.project_name.trim(), storage_pool: f.defaults.storage_pool.trim() },
          name: f.name.trim(),
          zone_id: zoneId,
        },
        { key },
      ),
    { success: '节点已登记' },
  );
  if (!r) return;
  open.value = false;
  emit('created', r.data);
}
</script>

<template>
  <Drawer v-model:open="open" destroy-on-close title="登记节点" :width="640">
    <Alert
      class="mb-4"
      message="新节点为「维护」、健康「未知」。登记后需要先探测库存，确认连接与库存正常后再启用。"
      show-icon
      type="info"
    />
    <Form ref="formRef" layout="vertical" :model="form">
      <Row :gutter="12">
        <Col :span="12">
          <FormItem label="名称" name="name" :rules="nameRules">
            <Input v-model:value="form.name" :maxlength="128" />
          </FormItem>
        </Col>
        <Col :span="12">
          <FormItem label="可用区" name="zone_id" :rules="[{ message: '请选择可用区', required: true }]">
            <Select
              v-model:value="form.zone_id"
              :options="props.zones.filter((z) => z.status === 'active').map((z) => ({ label: `${z.name}（${z.code}）`, value: z.id }))"
              placeholder="仅启用的可用区"
            />
          </FormItem>
        </Col>
      </Row>
      <Divider class="my-2" orientation="left" plain>连接</Divider>
      <ConnectionFields v-model="form.connection" :prefix="['connection']" />
      <Divider class="my-2" orientation="left" plain>默认值</Divider>
      <Row :gutter="12">
        <Col :span="12">
          <FormItem label="存储池" :name="['defaults', 'storage_pool']" :rules="poolRules">
            <Input v-model:value="form.defaults.storage_pool" class="font-mono" />
          </FormItem>
        </Col>
        <Col :span="12">
          <FormItem label="Incus 项目" :name="['defaults', 'project_name']" :rules="projectRules">
            <Input v-model:value="form.defaults.project_name" class="font-mono" />
          </FormItem>
        </Col>
      </Row>
      <Divider class="my-2" orientation="left" plain>可分配量</Divider>
      <AllocatableFields v-model="form.allocatable" :prefix="['allocatable']" />
    </Form>
    <template #footer>
      <div class="flex justify-end gap-2">
        <Button @click="open = false">取消</Button>
        <Button :loading="action.pending.value" type="primary" @click="submit">登记</Button>
      </div>
    </template>
  </Drawer>
</template>
