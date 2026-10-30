<script lang="ts" setup>
import type { FormInstance } from 'ant-design-vue';

import type { NetworkPoolView } from './network';

import type { NetworkMode, Node } from '#/api/types';

import { computed, ref, shallowRef, watch } from 'vue';

import { Alert, AutoComplete, Button, Drawer, Form, FormItem, Input, Select } from 'ant-design-vue';

import { networkApi, nodesApi } from '#/api/backend';
import { useAction } from '#/composables/api';

import { NETWORK_NAME_RE } from './network';

const props = defineProps<{ initialNodeId?: string; nodes: Node[]; pools: NetworkPoolView[] }>();
const emit = defineEmits<{ created: [] }>();
const open = defineModel<boolean>('open', { default: false });

interface BindingForm {
  incus_network_name: string;
  mode: NetworkMode;
  node_id: string | undefined;
  parent_interface: string;
  pool_id: string | undefined;
  project_name: string;
}

const form = ref<BindingForm>(blank());
const formRef = ref<FormInstance>();
const interfaces = shallowRef<string[]>([]);
const action = useAction();
watch(form, () => action.resetKey(), { deep: true });

function blank(): BindingForm {
  return {
    incus_network_name: '',
    mode: 'macvlan',
    node_id: undefined,
    parent_interface: '',
    pool_id: undefined,
    project_name: '',
  };
}

watch(open, (v) => {
  if (!v) return;
  const initial = props.nodes.find((n) => n.id === props.initialNodeId);
  form.value = { ...blank(), node_id: initial?.id, project_name: initial?.defaults.project_name ?? '' };
});

const node = computed(() => props.nodes.find((n) => n.id === form.value.node_id));

/** 选择节点后：项目默认取节点默认项目；父网卡候选来自该节点最近一次库存。 */
watch(node, async (n) => {
  interfaces.value = [];
  if (!n) return;
  form.value.project_name = n.defaults.project_name;
  if (form.value.pool_id && !props.pools.some((p) => p.id === form.value.pool_id && p.availability_zone_id === n.zone_id)) {
    form.value.pool_id = undefined;
  }
  try {
    const r = await nodesApi.resources(n.id);
    interfaces.value = (r.data.resources?.network_interfaces ?? []).filter((i) => !i.loopback).map((i) => i.name);
  } catch {
    // 候选列表仅为输入辅助，取不到时手工填写
  }
});

const nodeOptions = computed(() => props.nodes.map((n) => ({ label: `${n.name} #${n.id}`, value: n.id })));
const poolOptions = computed(() =>
  props.pools
    .filter((p) => !node.value || p.availability_zone_id === node.value.zone_id)
    .map((p) => ({ label: `${p.name}（${p.cidr}）`, value: p.id })),
);
const modeOptions = [
  { label: 'macvlan 直连', value: 'macvlan' },
  { label: '网桥 bridge', value: 'bridge' },
  { label: '托管网桥 managed_bridge', value: 'managed_bridge' },
];
const usesParent = computed(() => form.value.mode !== 'managed_bridge');

const nameRule = (label: string) => [
  { message: `请填写${label}`, required: true },
  { message: '1–63 字符，只含字母、数字、_ - .', pattern: NETWORK_NAME_RE },
];

async function submit() {
  try {
    await formRef.value?.validate();
  } catch {
    return;
  }
  const f = form.value;
  if (!f.node_id || !f.pool_id) return;
  const body = {
    mode: f.mode,
    node_id: f.node_id,
    pool_id: f.pool_id,
    project_name: f.project_name.trim(),
    ...(usesParent.value
      ? { parent_interface: f.parent_interface.trim() }
      : { incus_network_name: f.incus_network_name.trim() }),
  };
  const r = await action.run((key) => networkApi.createBinding(body, { key }), { success: '绑定已创建，请验证后启用' });
  if (!r) return;
  open.value = false;
  emit('created');
}
</script>

<template>
  <Drawer v-model:open="open" destroy-on-close title="创建节点网络绑定" :width="520">
    <Alert
      class="mb-4"
      message="只登记绑定关系，不修改宿主机网络。新绑定为停用、待验证；验证通过后才能启用。"
      show-icon
      type="info"
    />
    <Form ref="formRef" layout="vertical" :model="form">
      <FormItem label="节点" name="node_id" :rules="[{ message: '请选择节点', required: true }]">
        <Select v-model:value="form.node_id" option-filter-prop="label" :options="nodeOptions" show-search />
      </FormItem>
      <FormItem
        extra="只列出与节点同一可用区的地址池"
        label="地址池"
        name="pool_id"
        :rules="[{ message: '请选择地址池', required: true }]"
      >
        <Select v-model:value="form.pool_id" :options="poolOptions" />
      </FormItem>
      <FormItem label="Incus 项目" name="project_name" :rules="nameRule('Incus 项目')">
        <Input v-model:value="form.project_name" class="font-mono" />
      </FormItem>
      <FormItem extra="虚拟机不能使用 macvlan 绑定" label="模式" name="mode">
        <Select v-model:value="form.mode" :options="modeOptions" />
      </FormItem>
      <FormItem v-if="usesParent" label="父网卡" name="parent_interface" :rules="nameRule('父网卡')">
        <AutoComplete
          v-model:value="form.parent_interface"
          class="font-mono"
          :options="interfaces.map((i) => ({ value: i }))"
          placeholder="如 eth0、br0"
        />
      </FormItem>
      <FormItem v-else label="Incus 网络名" name="incus_network_name" :rules="nameRule('Incus 网络名')">
        <Input v-model:value="form.incus_network_name" class="font-mono" placeholder="如 incusbr0" />
      </FormItem>
    </Form>
    <template #footer>
      <div class="flex justify-end gap-2">
        <Button @click="open = false">取消</Button>
        <Button :loading="action.pending.value" type="primary" @click="submit">创建</Button>
      </div>
    </template>
  </Drawer>
</template>
