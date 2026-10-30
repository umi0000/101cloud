<script lang="ts" setup>
import type { PowerMode } from '#/api/backend';
import type { Instance, InstanceNetwork, Plan } from '#/api/types';

import { computed, onMounted, ref, shallowRef, watch } from 'vue';
import { useRoute } from 'vue-router';

import { Page } from '@vben/common-ui';

import {
  Button,
  Card,
  Descriptions,
  DescriptionsItem,
  Empty,
  Input,
  Modal,
  RadioButton,
  RadioGroup,
  Result,
  Space,
  Spin,
  TabPane,
  Table,
  Tabs,
  Tag,
  TypographyText,
} from 'ant-design-vue';

import { catalogApi, instancesApi, nodesApi } from '#/api/backend';
import { ApiError } from '#/api/request';
import ConfirmAction from '#/components/confirm-action.vue';
import StatusTag from '#/components/status-tag.vue';
import TimeText from '#/components/time-text.vue';
import UserName from '#/components/user-name.vue';
import { errorToast, trackAccepted, useAction, usePolling, useResource } from '#/composables/api';
import { enumLabel, instanceState } from '#/locales/enums';
import { useTaskStore } from '#/store/tasks';
import { formatAge, formatBytes, secondsSince } from '#/utils/format';

import ConsoleDrawer from './components/console-drawer.vue';
import GpuPanel from './components/gpu-panel.vue';
import GpuPolicyPanel from './components/gpu-policy-panel.vue';
import GraceTag from './components/grace-tag.vue';
import MetricsPanel from './components/metrics-panel.vue';
import ResizeDialog from './components/resize-dialog.vue';
import { useNow } from './components/shared';
import TaskPanel from './components/task-panel.vue';

const route = useRoute();
const id = route.params.id as string;

const inst = useResource(() => instancesApi.get(id));
const network = shallowRef<InstanceNetwork[]>([]);
const notFound = ref(false);

async function loadNetwork() {
  network.value = (await instancesApi.network(id)).data;
}

async function reload() {
  await Promise.all([inst.reload(), loadNetwork()]);
}

async function initial() {
  try {
    await reload();
  } catch (error) {
    if (error instanceof ApiError && error.status === 404) notFound.value = true;
    else errorToast(error);
  }
}
onMounted(initial);

const i = computed(() => inst.data.value);
usePolling(reload, () => (i.value && (i.value.active_task_id || i.value.provisioning_state !== 'ready') ? 5000 : 30_000));

const tasks = useTaskStore();
watch(
  () => i.value?.active_task_id,
  (task) => {
    if (task) tasks.track(task, '实例操作', () => void reload().catch(errorToast));
  },
);

/* ---------- 关联名称：镜像、套餐、节点 ---------- */

const imageName = ref('');
const plan = shallowRef<null | Plan>(null);
const planMissing = ref(false);
const nodeName = ref('');

watch(
  () => i.value?.image_id,
  async (imageId) => {
    if (!imageId) return;
    imageName.value = `镜像 #${imageId}`;
    try {
      imageName.value = (await catalogApi.image(imageId)).data.name;
    } catch {
      // 查不到保留"镜像 #id"
    }
  },
);
watch(
  () => i.value?.plan_id,
  async (planId) => {
    plan.value = null;
    planMissing.value = false;
    if (!planId) return;
    try {
      plan.value = (await catalogApi.plan(planId)).data;
    } catch (error) {
      if (error instanceof ApiError && error.status === 404) planMissing.value = true;
      else errorToast(error);
    }
  },
);
watch(
  () => i.value?.node_id,
  async (nodeId) => {
    if (!nodeId) return;
    nodeName.value = `节点 #${nodeId}`;
    try {
      nodeName.value = (await nodesApi.get(nodeId)).data.name;
    } catch {
      // 保留"节点 #id"
    }
  },
);

const planText = computed(() => {
  const p = i.value?.plan_id;
  if (!p) return '—';
  if (plan.value) return `${plan.value.name}（${plan.value.code} v${plan.value.version}）`;
  return planMissing.value ? `套餐 #${p}（已下线）` : `套餐 #${p}`;
});

const now = useNow(5000);
const age = computed(() => {
  void now.value;
  return formatAge(secondsSince(i.value?.observed_at));
});

/* ---------- 操作 ---------- */

const busy = computed(() => !!i.value?.active_task_id);
const ifMatch = () => inst.etag.value;
const conflict = () => reload().catch(errorToast);

function afterAccepted(accepted: Awaited<ReturnType<typeof instancesApi.power>> | undefined, label: string) {
  if (!accepted) return false;
  trackAccepted(accepted.data, label, () => void reload().catch(errorToast));
  void reload().catch(errorToast);
  return true;
}

// 重命名
const renameOpen = ref(false);
const renameValue = ref('');
const renameAction = useAction();
function openRename() {
  renameValue.value = i.value?.name ?? '';
  renameOpen.value = true;
}
const renameValid = computed(() => {
  const n = renameValue.value.trim();
  return n.length > 0 && [...n].length <= 128;
});
async function submitRename() {
  if (!renameValid.value) return;
  const r = await renameAction.run(() => instancesApi.rename(id, renameValue.value.trim(), { ifMatch: ifMatch() }), {
    onConflict: conflict,
    success: '已重命名',
  });
  if (!r) return;
  renameOpen.value = false;
  await reload().catch(errorToast);
}

// 启动 / 停止 / 重启
type PowerKind = 'restart' | 'start' | 'stop';
const POWER_NAMES: Record<PowerKind, string> = { restart: '重启', start: '启动', stop: '停止' };
const powerActions = { restart: useAction(), start: useAction(), stop: useAction() };
const powerOpen = ref(false);
const powerKind = ref<PowerKind>('start');
const powerMode = ref<PowerMode>('graceful');
function openPower(kind: PowerKind) {
  powerKind.value = kind;
  powerMode.value = 'graceful';
  powerActions[kind].resetKey();
  powerOpen.value = true;
}
watch(powerMode, () => powerActions[powerKind.value].resetKey());
async function submitPower() {
  const kind = powerKind.value;
  const body =
    kind === 'start'
      ? {}
      : powerMode.value === 'force'
        ? { confirm_data_loss_risk: true, mode: 'force' as const }
        : { mode: 'graceful' as const };
  const r = await powerActions[kind].run((key) => instancesApi.power(id, kind, body, { ifMatch: ifMatch(), key }), {
    onConflict: conflict,
    success: `已提交${POWER_NAMES[kind]}`,
  });
  return afterAccepted(r, `${POWER_NAMES[kind]}实例`);
}

// 调整规格
const resizeOpen = ref(false);

// 删除
const deleteOpen = ref(false);
const deleteAction = useAction();
function openDelete() {
  deleteAction.resetKey();
  deleteOpen.value = true;
}
async function submitDelete() {
  const name = i.value?.name ?? '';
  const r = await deleteAction.run(
    (key) => instancesApi.delete(id, { confirm_name: name, delete_data: true }, { ifMatch: ifMatch(), key }),
    { onConflict: conflict, success: '已提交删除' },
  );
  return afterAccepted(r, '删除实例');
}

// 控制台
const consoleRef = ref<InstanceType<typeof ConsoleDrawer>>();

/* ---------- 标签页 ---------- */

const tab = ref('overview');
const isContainer = computed(() => i.value?.instance_type === 'container');
const networkColumns = [
  { key: 'ip', title: 'IP' },
  { dataIndex: 'gateway', key: 'gateway', title: '网关' },
  { key: 'dns', title: 'DNS' },
  { key: 'mode', title: '模式' },
  { dataIndex: 'status', key: 'status', title: '状态' },
  { key: 'bound', title: '配置已绑定' },
  { key: 'observed', title: '观测时间' },
];

function resourcesText(x: Instance) {
  return `${x.resources.vcpus} vCPU / ${formatBytes(x.resources.memory_bytes)} / ${formatBytes(x.resources.disk_bytes)}`;
}
</script>

<template>
  <Page :title="i ? i.name : '实例详情'">
    <Result v-if="notFound" status="404" sub-title="实例不存在或已删除" title="未找到实例">
      <template #extra><RouterLink to="/admin/instances">返回实例列表</RouterLink></template>
    </Result>
    <Spin v-else-if="!i" />
    <template v-else>
      <Card class="mb-3" size="small">
        <div class="flex flex-wrap items-start justify-between gap-3">
          <div class="min-w-0">
            <div class="flex flex-wrap items-center gap-2">
              <span class="text-lg font-medium">{{ i.name }}</span>
              <Button size="small" type="link" @click="openRename">重命名</Button>
              <StatusTag :label="instanceState(i)" />
              <Tag v-if="i.product_type === 'custom'" class="m-0" color="processing">自定义</Tag>
              <Tag v-if="i.billing_mode === 'admin_exempt'" class="m-0" color="cyan">免额度</Tag>
              <GraceTag :enforcement="i.credit_enforcement" />
            </div>
            <div class="text-muted-foreground mt-1 flex flex-wrap gap-x-4 gap-y-1 text-sm">
              <span>所有者 <UserName :id="i.user_id" link /></span>
              <span>节点 <RouterLink :to="`/admin/nodes/${i.node_id}`">{{ nodeName }}</RouterLink></span>
              <span>{{ enumLabel('instanceType', i.instance_type).text }}</span>
              <span>{{ imageName }}</span>
              <span v-if="i.plan_id">{{ planText }}</span>
              <span v-if="network[0]">
                IP <TypographyText class="font-mono" :copyable="{ text: network[0].ip }">{{ network[0].ip }}</TypographyText>
              </span>
              <span>状态更新于 {{ age }}</span>
            </div>
          </div>
          <Space wrap>
            <template v-if="busy">
              <Tag class="m-0" color="processing">操作进行中</Tag>
              <RouterLink :to="`/admin/tasks?instance_id=${i.id}`">查看任务</RouterLink>
            </template>
            <Button v-if="i.capabilities.console" :disabled="busy" @click="consoleRef?.start()">
              {{ i.instance_type === 'virtual-machine' ? '控制台' : '终端' }}
            </Button>
            <span v-if="i.state_stale" class="text-sm text-orange-500">状态未知，等待同步</span>
            <template v-else>
              <Button v-if="i.capabilities.start" :disabled="busy" type="primary" @click="openPower('start')">启动</Button>
              <Button v-if="i.observed_state === 'running'" :disabled="busy" @click="openPower('restart')">重启</Button>
              <Button v-if="i.capabilities.stop" :disabled="busy" @click="openPower('stop')">停止</Button>
              <Button v-if="i.observed_state === 'stopped'" :disabled="busy" @click="resizeOpen = true">调整规格</Button>
              <Button v-if="i.observed_state === 'stopped'" danger :disabled="busy" @click="openDelete">删除</Button>
            </template>
          </Space>
        </div>
      </Card>

      <Tabs v-model:active-key="tab" destroy-inactive-tab-pane size="small">
        <TabPane key="overview" tab="概览">
          <div class="grid grid-cols-1 gap-3 lg:grid-cols-2">
            <Card size="small" title="配置">
              <Descriptions :column="1" size="small">
                <DescriptionsItem label="ID"><span class="font-mono">{{ i.id }}</span></DescriptionsItem>
                <DescriptionsItem label="规格"><span class="font-mono">{{ resourcesText(i) }}</span></DescriptionsItem>
                <DescriptionsItem label="类型">{{ enumLabel('instanceType', i.instance_type).text }}</DescriptionsItem>
                <DescriptionsItem label="产品类型"><StatusTag kind="productType" :value="i.product_type" /></DescriptionsItem>
                <DescriptionsItem label="计费方式"><StatusTag kind="billingMode" :value="i.billing_mode" /></DescriptionsItem>
                <DescriptionsItem label="套餐">{{ planText }}</DescriptionsItem>
                <DescriptionsItem label="镜像">{{ imageName }}</DescriptionsItem>
                <DescriptionsItem label="期望状态">{{ enumLabel('observed', i.desired_state).text }}</DescriptionsItem>
                <DescriptionsItem label="创建时间"><TimeText :value="i.created_at" /></DescriptionsItem>
                <DescriptionsItem label="更新时间"><TimeText :value="i.updated_at" /></DescriptionsItem>
              </Descriptions>
            </Card>
            <div class="flex flex-col gap-3">
              <Card size="small" title="网络">
                <Empty v-if="network.length === 0" description="没有网络地址" />
                <div v-for="n in network" :key="n.ip" class="mb-2 text-sm last:mb-0">
                  <div class="font-mono">{{ n.ip }}/{{ n.prefix_length }} · 网关 {{ n.gateway }}</div>
                  <div class="text-muted-foreground">
                    DNS <span class="font-mono">{{ n.dns.join(', ') || '—' }}</span> ·
                    {{ enumLabel('networkMode', n.mode).text }} · {{ enumLabel('addressStatus', n.status).text }}
                  </div>
                  <div v-if="n.quality.state_stale" class="text-orange-500">网络状态可能已过期</div>
                </div>
              </Card>
              <Card size="small" title="计费">
                <div class="text-sm">
                  计费方式：<StatusTag kind="billingMode" :value="i.billing_mode" />
                </div>
                <div class="text-muted-foreground mt-1 text-xs">后端不提供实例当前总费率，这里不显示。</div>
              </Card>
            </div>
          </div>
        </TabPane>
        <TabPane key="metrics" tab="监控">
          <MetricsPanel :instance-id="i.id" />
        </TabPane>
        <TabPane v-if="isContainer" key="gpu" tab="GPU">
          <GpuPanel :etag="inst.etag.value" :instance="i" @changed="conflict" />
        </TabPane>
        <TabPane key="network" tab="网络">
          <Card size="small">
            <Table :columns="networkColumns" :data-source="network" :pagination="false" row-key="ip" size="small">
              <template #bodyCell="{ column, record }">
                <template v-if="column.key === 'ip'"><span class="font-mono">{{ record.ip }}/{{ record.prefix_length }}</span></template>
                <template v-else-if="column.key === 'gateway'"><span class="font-mono">{{ record.gateway }}</span></template>
                <template v-else-if="column.key === 'dns'"><span class="font-mono">{{ record.dns.join(', ') || '—' }}</span></template>
                <template v-else-if="column.key === 'mode'">{{ enumLabel('networkMode', record.mode).text }}</template>
                <template v-else-if="column.key === 'status'"><StatusTag kind="addressStatus" :value="record.status" /></template>
                <template v-else-if="column.key === 'bound'">
                  {{ record.quality.configuration_bound ? '是' : '否' }}
                  <Tag v-if="record.quality.state_stale" class="m-0 ml-1" color="warning">可能已过期</Tag>
                </template>
                <template v-else-if="column.key === 'observed'"><TimeText :value="record.observed_at" /></template>
              </template>
            </Table>
          </Card>
        </TabPane>
        <TabPane key="tasks" tab="任务">
          <TaskPanel :instance-id="i.id" />
        </TabPane>
        <TabPane v-if="isContainer" key="policy" tab="GPU 策略">
          <GpuPolicyPanel :instance="i" @changed="conflict" />
        </TabPane>
      </Tabs>

      <Modal
        v-model:open="renameOpen"
        :confirm-loading="renameAction.pending.value"
        :ok-button-props="{ disabled: !renameValid }"
        title="重命名实例"
        @ok="submitRename"
      >
        <Input v-model:value="renameValue" :maxlength="128" placeholder="1–128 个字符" />
      </Modal>

      <ConfirmAction
        v-model:open="powerOpen"
        :danger="powerKind !== 'start' && powerMode === 'force'"
        :description="powerKind === 'start' ? '启动会重新进行准入检查。' : powerMode === 'force' ? '强制操作不删除磁盘，但可能丢失未保存的数据。' : ''"
        :on-confirm="submitPower"
        :risk-text="powerKind !== 'start' && powerMode === 'force' ? '我知道强制操作可能丢失未保存数据' : ''"
        :title="`${POWER_NAMES[powerKind]}实例`"
      >
        <div v-if="powerKind !== 'start'" class="mb-3">
          <RadioGroup v-model:value="powerMode" button-style="solid">
            <RadioButton value="graceful">正常{{ POWER_NAMES[powerKind] }}</RadioButton>
            <RadioButton value="force">强制{{ POWER_NAMES[powerKind] }}</RadioButton>
          </RadioGroup>
        </div>
      </ConfirmAction>

      <ResizeDialog v-model:open="resizeOpen" :etag="inst.etag.value" :instance="i" :plan="plan" @done="conflict" />

      <ConfirmAction
        v-model:open="deleteOpen"
        :confirm-name="i.name"
        danger
        description="实例和磁盘数据将被永久删除。实例必须处于近期确认的已停止状态。"
        ok-text="删除"
        :on-confirm="submitDelete"
        title="删除实例"
      />

      <ConsoleDrawer ref="consoleRef" :etag="inst.etag.value" :instance="i" />
    </template>
  </Page>
</template>
