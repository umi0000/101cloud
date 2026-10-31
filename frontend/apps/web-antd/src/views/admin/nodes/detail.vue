<script lang="ts" setup>
import type { FormInstance } from 'ant-design-vue';

import type { AdministrativeState, NodeConnectionInput } from '#/api/types';

import { computed, onMounted, ref } from 'vue';
import { useRoute } from 'vue-router';

import { Page } from '@vben/common-ui';

import { Button, Card, Form, Input, message, Space, TabPane, Tabs, Tag, Tooltip } from 'ant-design-vue';

import { nodesApi } from '#/api/backend';
import ConfirmAction from '#/components/confirm-action.vue';
import StatusTag from '#/components/status-tag.vue';
import { errorToast, trackAccepted, useAction, useResource } from '#/composables/api';
import { formatAge, formatFullTime, secondsSince, shortId } from '#/utils/format';

import BindingsTab from './components/bindings-tab.vue';
import ConnectionFields from './components/connection-fields.vue';
import GpuTab from './components/gpu-tab.vue';
import InstancesTab from './components/instances-tab.vue';
import { connectionBody, emptyConnection } from './components/node-forms';
import OverviewTab from './components/overview-tab.vue';
import UnmanagedTab from './components/unmanaged-tab.vue';

const route = useRoute();
const id = String(route.params.id);
const node = useResource(() => nodesApi.get(id));
/** 探测完成、纳管完成后递增，通知各标签页重新加载。 */
const epoch = ref(0);
const tab = ref('overview');

async function reload() {
  try {
    await node.reload();
  } catch (error) {
    errorToast(error);
  }
}

onMounted(() => reload());

function refreshAll() {
  void reload();
  epoch.value++;
}

const inventoryAge = computed(() => secondsSince(node.data.value?.resources_sampled_at));
const inventoryStale = computed(() => inventoryAge.value === null || inventoryAge.value > 60);

/* ---------- 探测 ---------- */
const probeAction = useAction();
async function probe() {
  const r = await probeAction.run((key) => nodesApi.probe(id, { ifMatch: node.etag.value, key }), {
    onConflict: reload,
    success: '已提交探测',
  });
  if (!r) return;
  trackAccepted(r.data, `探测节点 ${node.data.value?.name ?? id}`, refreshAll);
}

/* ---------- 管理状态 ---------- */
const stateAction = useAction();
async function setState(state: AdministrativeState) {
  const r = await stateAction.run(
    () => nodesApi.update(id, { administrative_state: state }, { ifMatch: node.etag.value }),
    { onConflict: reload, success: '状态已更新' },
  );
  if (r) await reload();
  return !!r;
}

const stateConfirm = ref<'disabled' | 'maintenance' | null>(null);
const stateConfirmOpen = computed({
  get: () => stateConfirm.value !== null,
  set: (v) => {
    if (!v) stateConfirm.value = null;
  },
});
const stateConfirmText = computed(() =>
  stateConfirm.value === 'disabled'
    ? { description: '禁用后节点不再接受新的分配。不会停止或删除已有实例。', title: '禁用节点' }
    : { description: '维护状态下节点不再接受新的分配。不会停止或删除已有实例。', title: '设为维护' },
);
function confirmState() {
  return stateConfirm.value ? setState(stateConfirm.value) : false;
}

/* ---------- 排空 ---------- */
const drainOpen = ref(false);
const drainAction = useAction();
async function drain() {
  const r = await drainAction.run((key) => nodesApi.drain(id, { ifMatch: node.etag.value, key }), {
    onConflict: reload,
    success: '已提交排空',
  });
  if (!r) return false;
  trackAccepted(r.data, `排空节点 ${node.data.value?.name ?? id}`, refreshAll);
  await reload();
  return true;
}

/* ---------- 更换连接 ---------- */
const connOpen = ref(false);
const connForm = ref<{ connection: NodeConnectionInput }>({ connection: emptyConnection() });
const connRef = ref<FormInstance>();
const connAction = useAction();
function openConnection() {
  const n = node.data.value;
  connForm.value = { connection: emptyConnection(n?.connection.endpoint, n?.connection.server_cert_sha256) };
  connAction.resetKey();
  connOpen.value = true;
}
async function replaceConnection() {
  try {
    await connRef.value?.validate();
  } catch {
    return false;
  }
  const r = await connAction.run(
    () =>
      nodesApi.replaceConnection(
        id,
        { connection: connectionBody(connForm.value.connection) },
        { ifMatch: node.etag.value },
      ),
    { onConflict: reload, success: '连接已更换，请重新探测库存' },
  );
  if (!r) return false;
  refreshAll();
  return true;
}

/* ---------- 重命名 ---------- */
const renameOpen = ref(false);
const newName = ref('');
const renameAction = useAction();
function openRename() {
  newName.value = node.data.value?.name ?? '';
  renameOpen.value = true;
}
async function rename() {
  const name = newName.value.trim();
  if (!name || [...name].length > 128) {
    message.warning('名称为 1–128 字符');
    return false;
  }
  const r = await renameAction.run(() => nodesApi.update(id, { name }, { ifMatch: node.etag.value }), {
    onConflict: reload,
    success: '已重命名',
  });
  if (r) await reload();
  return !!r;
}
</script>

<template>
  <Page :title="node.data.value?.name ?? `节点 #${id}`">
    <template v-if="node.data.value" #title>
      <div class="mb-2 flex flex-wrap items-center gap-2 text-lg font-semibold">
        {{ node.data.value.name }}
        <span class="text-muted-foreground font-mono text-sm font-normal">#{{ node.data.value.id }}</span>
        <StatusTag kind="adminState" :value="node.data.value.administrative_state" />
        <StatusTag kind="health" :value="node.data.value.health" />
        <Tooltip v-if="node.data.value.connection_verified_at" :title="formatFullTime(node.data.value.connection_verified_at)">
          <Tag :bordered="false" color="success">证书已验证</Tag>
        </Tooltip>
        <Tag v-else :bordered="false" color="warning">连接未验证</Tag>
      </div>
    </template>
    <template v-if="node.data.value" #description>
      <div class="text-muted-foreground flex flex-wrap gap-x-5 gap-y-1 text-sm">
        <span class="font-mono">{{ node.data.value.connection.endpoint }}</span>
        <Tooltip :title="node.data.value.connection.server_cert_sha256">
          <span>证书 <span class="font-mono">{{ shortId(node.data.value.connection.server_cert_sha256, 12, 6) }}</span></span>
        </Tooltip>
        <span>密钥版本 <span class="font-mono">{{ node.data.value.connection.key_version }}</span></span>
        <Tooltip :title="formatFullTime(node.data.value.resources_sampled_at)">
          <span :class="{ 'text-orange-500': inventoryStale }">
            库存 {{ node.data.value.resources_sampled_at ? formatAge(inventoryAge) : '无' }}
          </span>
        </Tooltip>
      </div>
    </template>
    <template v-if="node.data.value" #extra>
      <Space wrap>
        <Button :loading="probeAction.pending.value" type="primary" @click="probe">探测库存</Button>
        <template v-if="node.data.value.administrative_state !== 'active'">
          <Button :loading="stateAction.pending.value" @click="setState('active')">启用</Button>
          <span v-if="inventoryStale" class="text-xs text-orange-500">建议先探测库存</span>
        </template>
        <Button v-if="node.data.value.administrative_state !== 'maintenance'" @click="stateConfirm = 'maintenance'">
          维护
        </Button>
        <Button v-if="node.data.value.administrative_state !== 'disabled'" @click="stateConfirm = 'disabled'">禁用</Button>
        <Button v-if="node.data.value.administrative_state !== 'draining'" danger @click="drainOpen = true">排空</Button>
        <Button @click="openConnection">更换连接</Button>
        <Button @click="openRename">重命名</Button>
      </Space>
    </template>

    <Card v-if="node.data.value" size="small">
      <Tabs v-model:active-key="tab" destroy-inactive-tab-pane>
        <TabPane key="overview" tab="概览">
          <OverviewTab :epoch="epoch" :etag="node.etag.value" :node="node.data.value" @changed="reload" />
        </TabPane>
        <TabPane key="gpus" tab="GPU">
          <GpuTab :epoch="epoch" :node="node.data.value" />
        </TabPane>
        <TabPane key="bindings" tab="网络绑定">
          <BindingsTab :node-id="id" />
        </TabPane>
        <TabPane key="unmanaged" tab="未纳管实例">
          <UnmanagedTab :epoch="epoch" :etag="node.etag.value" :node="node.data.value" @imported="refreshAll" />
        </TabPane>
        <TabPane key="instances" tab="实例">
          <InstancesTab :epoch="epoch" :node-id="id" />
        </TabPane>
      </Tabs>
    </Card>

    <ConfirmAction
      v-model:open="stateConfirmOpen"
      :description="stateConfirmText.description"
      :on-confirm="confirmState"
      :title="stateConfirmText.title"
    />
    <ConfirmAction
      v-model:open="drainOpen"
      danger
      description="禁止新的分配并盘点库存。不迁移、不停机，已有实例继续运行。"
      ok-text="排空"
      :on-confirm="drain"
      title="排空节点"
    />
    <ConfirmAction
      v-model:open="connOpen"
      danger
      description="替换加密凭据。后端会清空连接验证状态与库存，启用中的节点转为维护；更换后需要重新探测库存。"
      ok-text="更换"
      :on-confirm="replaceConnection"
      title="更换连接"
    >
      <Form ref="connRef" layout="vertical" :model="connForm">
        <ConnectionFields v-model="connForm.connection" :prefix="['connection']" />
      </Form>
    </ConfirmAction>
    <ConfirmAction
      v-model:open="renameOpen"
      :on-confirm="rename"
      title="重命名节点"
    >
      <Input v-model:value="newName" :maxlength="128" />
    </ConfirmAction>
  </Page>
</template>
