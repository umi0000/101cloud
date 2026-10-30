<script lang="ts" setup>
import type { Node, NodeGPU, PolicyVersion } from '#/api/types';

import { computed, ref, watch } from 'vue';

import { Alert, Button, Descriptions, DescriptionsItem, Drawer, Form, FormItem, InputNumber, Select } from 'ant-design-vue';

import { nodesApi } from '#/api/backend';
import StatusTag from '#/components/status-tag.vue';
import { errorToast, useAction, useResource } from '#/composables/api';

import { gpuPriceLabel, priceConfig, priceEffective } from './node-forms';

const props = defineProps<{ gpuId: null | string; node: Node; prices: PolicyVersion[] }>();
const emit = defineEmits<{ saved: [] }>();
const open = defineModel<boolean>('open', { default: false });

type EditableStatus = 'available' | 'disabled' | 'maintenance';
const EDITABLE: EditableStatus[] = ['available', 'maintenance', 'disabled'];
const statusOptions = [
  { label: '可用', value: 'available' },
  { label: '维护', value: 'maintenance' },
  { label: '禁用', value: 'disabled' },
];

const gpu = useResource(() => nodesApi.gpu(props.node.id, props.gpuId ?? ''));
interface GPUForm {
  gpu_price_version_id: string | undefined;
  max_allocations: number | undefined;
  status: EditableStatus | undefined;
}
const form = ref<GPUForm>({ gpu_price_version_id: undefined, max_allocations: undefined, status: undefined });
const action = useAction();
watch(form, () => action.resetKey(), { deep: true });

function editable(s: NodeGPU['status']): s is EditableStatus {
  return EDITABLE.some((e) => e === s);
}

async function load() {
  if (!props.gpuId) return;
  try {
    await gpu.reload();
  } catch (error) {
    errorToast(error);
    return;
  }
  const g = gpu.data.value;
  if (!g) return;
  form.value = {
    gpu_price_version_id: g.gpu_price_version_id ?? undefined,
    max_allocations: g.max_allocations,
    status: editable(g.status) ? g.status : undefined,
  };
}
watch(open, (v) => {
  if (v) void load();
});

const priceOptions = computed(() => {
  const g = gpu.data.value;
  if (!g) return [];
  return props.prices
    .filter((p) => (p.status === 'active' && priceConfig(p).model === g.model) || p.id === g.gpu_price_version_id)
    .map((p) => {
      const cfg = priceConfig(p);
      const usable = priceEffective(p) && cfg.model === g.model;
      const mode = cfg.accessMode === 'exclusive' ? '独占' : '共享';
      return {
        disabled: !usable,
        label: `${gpuPriceLabel(p)} · ${mode}${usable ? '' : '（不可用）'}`,
        value: p.id,
      };
    });
});

const selectedPrice = computed(() => props.prices.find((p) => p.id === form.value.gpu_price_version_id));
const exclusiveMismatch = computed(
  () =>
    !!selectedPrice.value &&
    priceConfig(selectedPrice.value).accessMode === 'exclusive' &&
    form.value.max_allocations !== 1,
);

async function submit() {
  const g = gpu.data.value;
  if (!g || !props.gpuId) return;
  const gpuId = props.gpuId;
  const body: Partial<{ gpu_price_version_id: string; max_allocations: number; status: EditableStatus }> = {};
  const f = form.value;
  if (f.status && f.status !== g.status) body.status = f.status;
  if (typeof f.max_allocations === 'number' && f.max_allocations !== g.max_allocations) body.max_allocations = f.max_allocations;
  if (f.gpu_price_version_id && f.gpu_price_version_id !== g.gpu_price_version_id) {
    body.gpu_price_version_id = f.gpu_price_version_id;
  }
  if (Object.keys(body).length === 0) {
    open.value = false;
    return;
  }
  const r = await action.run(() => nodesApi.updateGPU(props.node.id, gpuId, body, { ifMatch: gpu.etag.value }), {
    onConflict: () => load(),
    success: 'GPU 已更新',
  });
  if (!r) return;
  open.value = false;
  emit('saved');
}
</script>

<template>
  <Drawer v-model:open="open" destroy-on-close title="编辑 GPU" :width="560">
    <template v-if="gpu.data.value">
      <Descriptions bordered class="mb-4" :column="1" size="small">
        <DescriptionsItem label="型号">{{ gpu.data.value.model }}</DescriptionsItem>
        <DescriptionsItem label="PCI"><span class="font-mono">{{ gpu.data.value.pci_address }}</span></DescriptionsItem>
        <DescriptionsItem label="UUID"><span class="font-mono text-xs">{{ gpu.data.value.uuid }}</span></DescriptionsItem>
        <DescriptionsItem label="当前状态"><StatusTag kind="gpuStatus" :value="gpu.data.value.status" /></DescriptionsItem>
        <DescriptionsItem label="活动分配">{{ gpu.data.value.allocation_count }}</DescriptionsItem>
      </Descriptions>
      <Form layout="vertical" :model="form">
        <FormItem
          :extra="editable(gpu.data.value.status) ? undefined : '「未找到」「错误」由探测设置；不选择则保持当前状态'"
          label="状态"
        >
          <Select v-model:value="form.status" allow-clear :options="statusOptions" placeholder="保持当前状态" />
        </FormItem>
        <FormItem extra="不能小于当前活动分配数" label="分配名额 max_allocations">
          <InputNumber
            v-model:value="form.max_allocations"
            class="w-full"
            :min="Math.max(1, gpu.data.value.allocation_count)"
            :precision="0"
          />
        </FormItem>
        <FormItem extra="只能选择已发布、已生效且型号与本卡一致的 GPU 定价版本" label="GPU 定价">
          <Select
            v-model:value="form.gpu_price_version_id"
            :not-found-content="`没有型号为 ${gpu.data.value.model} 的已发布 GPU 定价`"
            :options="priceOptions"
            placeholder="未定价"
          />
        </FormItem>
      </Form>
      <Alert v-if="exclusiveMismatch" class="mb-3" message="独占定价要求分配名额为 1。" show-icon type="warning" />
      <Alert
        v-if="form.status === 'available'"
        message="设为可用需要已有定价，且节点在线、连接已验证、该卡有当前健康观测；否则后端拒绝（观测不完整）。"
        show-icon
        type="info"
      />
    </template>
    <template #footer>
      <div class="flex justify-end gap-2">
        <Button @click="open = false">取消</Button>
        <Button :disabled="!gpu.data.value || exclusiveMismatch" :loading="action.pending.value" type="primary" @click="submit">
          保存
        </Button>
      </div>
    </template>
  </Drawer>
</template>
