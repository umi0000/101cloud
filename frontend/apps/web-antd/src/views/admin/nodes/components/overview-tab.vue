<script lang="ts" setup>
import type { FormInstance } from 'ant-design-vue';

import type { Node } from '#/api/types';

import type { AllocatableForm } from './node-forms';

import { computed, onMounted, ref, watch } from 'vue';

import { Alert, Button, Card, Descriptions, DescriptionsItem, Empty, Form, Space, Table, Tag } from 'ant-design-vue';

import { nodesApi } from '#/api/backend';
import StatusTag from '#/components/status-tag.vue';
import TimeText from '#/components/time-text.vue';
import { errorToast, useAction, useResource } from '#/composables/api';
import { formatBytes, formatPercent } from '#/utils/format';

import AllocatableFields from './allocatable-fields.vue';
import { allocatableFromForm, allocatableToForm } from './node-forms';

const props = defineProps<{ epoch: number; etag: null | string; node: Node }>();
const emit = defineEmits<{ changed: [] }>();

const res = useResource(() => nodesApi.resources(props.node.id));
async function loadResources() {
  try {
    await res.reload();
  } catch (error) {
    errorToast(error);
  }
}
onMounted(loadResources);
watch(() => props.epoch, loadResources);

/* ---------- 可分配量编辑 ---------- */
const editing = ref(false);
const form = ref<AllocatableForm>(allocatableToForm(props.node.allocatable));
const formRef = ref<FormInstance>();
const action = useAction();
watch(form, () => action.resetKey(), { deep: true });

function startEdit() {
  form.value = allocatableToForm(props.node.allocatable);
  editing.value = true;
}

async function save() {
  try {
    await formRef.value?.validate();
  } catch {
    return;
  }
  const r = await action.run(
    () => nodesApi.update(props.node.id, { allocatable: allocatableFromForm(form.value) }, { ifMatch: props.etag }),
    { onConflict: () => emit('changed'), success: '可分配量已更新' },
  );
  if (!r) return;
  editing.value = false;
  emit('changed');
}

/* ---------- 宿主实测 ---------- */
const server = computed(() => res.data.value?.resources?.server_resources);
const interfaces = computed(() => res.data.value?.resources?.network_interfaces ?? []);
const disks = computed(() => server.value?.storage?.disks ?? []);

/** resources 中的字节数是 JSON 数字；缺失显示"未知"。 */
function bytes(n: number | undefined) {
  if (n === undefined || n === null || !Number.isFinite(n)) return '未知';
  return formatBytes(String(Math.trunc(n)));
}
const memoryText = computed(() => {
  const m = server.value?.memory;
  if (m?.total === undefined) return '未知';
  const used = m.used === undefined ? '未知' : bytes(m.used);
  const pct = m.used !== undefined && m.total > 0 ? `（${formatPercent((m.used / m.total) * 100)}）` : '';
  return `${used} / ${bytes(m.total)}${pct}`;
});

const diskColumns = [
  { dataIndex: 'id', title: '设备' },
  { dataIndex: 'model', title: '型号' },
  { dataIndex: 'type', title: '类型', width: 90 },
  { key: 'size', title: '容量', width: 110 },
];
const nicColumns = [
  { dataIndex: 'name', title: '名称' },
  { key: 'up', title: '状态', width: 80 },
  { dataIndex: 'mtu', title: 'MTU', width: 80 },
  { key: 'mac', title: 'MAC' },
  { key: 'addresses', title: '地址' },
];
</script>

<template>
  <div class="flex flex-col gap-3">
    <Card size="small" title="可分配量">
      <template #extra>
        <Space v-if="editing">
          <Button size="small" @click="editing = false">取消</Button>
          <Button :loading="action.pending.value" size="small" type="primary" @click="save">保存</Button>
        </Space>
        <Button v-else size="small" @click="startEdit">编辑</Button>
      </template>
      <Form v-if="editing" ref="formRef" layout="vertical" :model="form">
        <AllocatableFields v-model="form" />
        <div class="text-muted-foreground text-xs">不能低于已有实例占用的资源（否则返回"容量已被占用"）。</div>
      </Form>
      <Descriptions v-else bordered :column="3" size="small">
        <DescriptionsItem label="vCPU">{{ node.allocatable.vcpus }}</DescriptionsItem>
        <DescriptionsItem label="内存">{{ formatBytes(node.allocatable.memory_bytes) }}</DescriptionsItem>
        <DescriptionsItem label="磁盘">{{ formatBytes(node.allocatable.disk_bytes) }}</DescriptionsItem>
        <DescriptionsItem label="常驻 vCPU">{{ node.allocatable.resident_vcpus }}</DescriptionsItem>
        <DescriptionsItem label="常驻内存">{{ formatBytes(node.allocatable.resident_memory_bytes) }}</DescriptionsItem>
        <DescriptionsItem label="常驻磁盘">{{ formatBytes(node.allocatable.resident_disk_bytes) }}</DescriptionsItem>
        <DescriptionsItem label="存储池"><span class="font-mono">{{ node.defaults.storage_pool }}</span></DescriptionsItem>
        <DescriptionsItem label="Incus 项目"><span class="font-mono">{{ node.defaults.project_name }}</span></DescriptionsItem>
      </Descriptions>
      <div class="text-muted-foreground mt-2 text-xs">后端不提供节点已分配资源汇总，这里只显示可分配量与宿主实测。</div>
    </Card>

    <Card size="small" title="宿主资源">
      <template #extra>
        <Space>
          <template v-if="res.data.value">
            <StatusTag kind="health" :value="res.data.value.health" />
            <StatusTag kind="quality" :value="res.data.value.quality" />
            <span class="text-muted-foreground text-xs">观测于 <TimeText :value="res.data.value.observed_at" /></span>
          </template>
          <Button :loading="res.loading.value" size="small" @click="loadResources">刷新</Button>
        </Space>
      </template>
      <Alert
        v-if="res.data.value?.quality === 'stale'"
        class="mb-3"
        message="库存已过期：节点不在线或连接未验证，以下为最后一次探测结果。"
        show-icon
        type="warning"
      />
      <Empty v-if="res.data.value && !res.data.value.resources" description="尚无库存数据，请先探测库存" />
      <template v-else-if="res.data.value?.resources">
        <Descriptions bordered :column="3" size="small">
          <DescriptionsItem label="CPU 总数">{{ server?.cpu?.total ?? '未知' }}</DescriptionsItem>
          <DescriptionsItem label="架构"><span class="font-mono">{{ server?.cpu?.architecture ?? '未知' }}</span></DescriptionsItem>
          <DescriptionsItem label="平均负载">{{ formatPercent(res.data.value.resources.average_load_percent) }}</DescriptionsItem>
          <DescriptionsItem label="内存（已用 / 总量）" :span="2">{{ memoryText }}</DescriptionsItem>
          <DescriptionsItem label="存储总量">{{ bytes(server?.storage?.total) }}</DescriptionsItem>
          <DescriptionsItem v-if="server?.system" label="系统" :span="3">
            {{ [server.system.vendor, server.system.product, server.system.type].filter(Boolean).join(' · ') || '未知' }}
          </DescriptionsItem>
        </Descriptions>
        <h4 class="mb-2 mt-4 font-medium">磁盘</h4>
        <Table :columns="diskColumns" :data-source="disks" :pagination="false" :row-key="(_r: unknown, i?: number) => String(i)" size="small">
          <template #bodyCell="{ column, record }">
            <template v-if="column.key === 'size'">{{ bytes(record.size) }}</template>
          </template>
        </Table>
        <h4 class="mb-2 mt-4 font-medium">网卡</h4>
        <Table :columns="nicColumns" :data-source="interfaces" :pagination="false" row-key="name" size="small">
          <template #bodyCell="{ column, record }">
            <template v-if="column.key === 'up'">
              <Tag :bordered="false" :color="record.up ? 'success' : 'default'">{{ record.up ? 'UP' : 'DOWN' }}</Tag>
            </template>
            <template v-else-if="column.key === 'mac'"><span class="font-mono text-xs">{{ record.mac || '—' }}</span></template>
            <template v-else-if="column.key === 'addresses'">
              <div v-for="a in record.addresses" :key="a.address" class="font-mono text-xs">
                {{ a.address }}{{ a.prefix_length !== undefined ? `/${a.prefix_length}` : '' }}
                <span class="text-muted-foreground">{{ a.family }}</span>
              </div>
              <span v-if="record.addresses.length === 0">—</span>
            </template>
          </template>
        </Table>
      </template>
    </Card>
  </div>
</template>
