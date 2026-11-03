<script lang="ts" setup>
import type { FormInstance } from 'ant-design-vue';

import type {
  AccessMode,
  CustomAction,
  CustomConfig,
  GPUOption,
  Image,
  NetworkOption,
  Node,
  PolicyKind,
  PolicyVersion,
  PolicyVersions,
  User,
} from '#/api/types';

import type { UnmanagedView } from './node-forms';

import { computed, ref, shallowRef, watch } from 'vue';

import {
  Alert,
  Button,
  CheckboxGroup,
  Col,
  Descriptions,
  DescriptionsItem,
  Divider,
  Drawer,
  Form,
  FormItem,
  Input,
  InputNumber,
  Row,
  Select,
  Switch,
} from 'ant-design-vue';

import { catalogApi, listAll, nodesApi, policiesApi, usersApi } from '#/api/backend';
import { CUSTOM_ACTIONS, GPU_USER_PERMISSIONS } from '#/api/types';
import ConfirmAction from '#/components/confirm-action.vue';
import { errorToast, trackAccepted, useAction } from '#/composables/api';
import { CUSTOM_ACTION_NAMES, enumLabel, GPU_PERMISSION_NAMES } from '#/locales/enums';
import { bytesToGib, gibToBytes, PT_PATTERN } from '#/utils/format';

const props = defineProps<{ etag: null | string; item: null | UnmanagedView; node: Node }>();
const emit = defineEmits<{ imported: [] }>();
const open = defineModel<boolean>('open', { default: false });

type Permission = (typeof GPU_USER_PERMISSIONS)[number];

interface ImportForm {
  access_mode: AccessMode;
  allowed_user_actions: CustomAction[];
  disk_gib: number | undefined;
  gpu_count: number;
  gpu_model: string | undefined;
  gpu_user_permissions: Record<Permission, boolean>;
  image_id: string | undefined;
  max_hourly_pt: string;
  memory_gib: number | undefined;
  network_option_id: string | undefined;
  owner_user_id: string | undefined;
  policy_versions: PolicyVersions;
  vcpus: number | undefined;
}

const form = ref<ImportForm>(blank());
const formRef = ref<FormInstance>();
/** 原生 limits.memory 的精确字节数；GiB 输入未改动时原样提交，避免换算误差导致核对失败。 */
const nativeMemoryBytes = ref<null | string>(null);

const users = shallowRef<User[]>([]);
const images = shallowRef<Image[]>([]);
const networks = shallowRef<NetworkOption[]>([]);
const gpuOptions = shallowRef<GPUOption[]>([]);
const profiles = shallowRef<PolicyVersion[]>([]);
const loading = ref(false);

const action = useAction();
watch(form, () => action.resetKey(), { deep: true });

function blank(): ImportForm {
  return {
    access_mode: 'shared',
    allowed_user_actions: [],
    disk_gib: undefined,
    gpu_count: 0,
    gpu_model: undefined,
    gpu_user_permissions: Object.fromEntries(GPU_USER_PERMISSIONS.map((p) => [p, false])) as Record<Permission, boolean>,
    image_id: undefined,
    max_hourly_pt: '0',
    memory_gib: undefined,
    network_option_id: undefined,
    owner_user_id: undefined,
    policy_versions: {},
    vcpus: undefined,
  };
}

const UNITS: Record<string, bigint> = {
  EiB: 1n << 60n,
  GB: 1_000_000_000n,
  GiB: 1n << 30n,
  KiB: 1n << 10n,
  MB: 1_000_000n,
  MiB: 1n << 20n,
  PiB: 1n << 50n,
  TB: 1_000_000_000_000n,
  TiB: 1n << 40n,
  kB: 1000n,
};

/** Incus 容量字符串（"8GiB"、"8192MiB"、"8589934592"）→ 字节字符串；无法精确解析返回 null。 */
function nativeBytes(value: unknown): null | string {
  if (typeof value !== 'string') return null;
  const m = /^(\d+)([A-Za-z]*)$/.exec(value.trim());
  const digits = m?.[1];
  if (!m || digits === undefined) return null;
  const unit = m[2] ? UNITS[m[2]] : 1n;
  return unit === undefined ? null : (BigInt(digits) * unit).toString();
}

function prefill(item: UnmanagedView) {
  const f = blank();
  const cpu = item.config['limits.cpu'];
  if (typeof cpu === 'string' && /^\d+$/.test(cpu)) f.vcpus = Number(cpu);
  nativeMemoryBytes.value = nativeBytes(item.config['limits.memory']);
  if (nativeMemoryBytes.value) f.memory_gib = bytesToGib(nativeMemoryBytes.value);
  form.value = f;
}

let optionsSeq = 0;
async function loadOptions(item: UnmanagedView) {
  const mine = ++optionsSeq;
  images.value = [];
  networks.value = [];
  gpuOptions.value = [];
  loading.value = true;
  try {
    const nodeId = props.node.id;
    // network_option_id 即绑定 id；后端要求绑定所在节点等于纳管节点，按 node_id 过滤。
    profiles.value = [];
    const [u, imgs, nets, gpus, policies] = await Promise.all([
      listAll(usersApi.list, { status: 'active' }),
      listAll(catalogApi.images, { instance_type: item.instance_type, node_id: nodeId, project_name: item.project_name }),
      listAll(catalogApi.networkOptions, { instance_type: item.instance_type, node_id: nodeId }),
      item.instance_type === 'container' ? listAll(catalogApi.gpuOptions, { node_id: nodeId }) : Promise.resolve([]),
      Promise.all((['pricing', 'credit', 'metrics', 'gpu-idle', 'gpu-scheduling', 'gpu-pricing'] as PolicyKind[])
        .map((kind) => listAll((q) => policiesApi.list(kind, q)))),
    ]);
    if (mine !== optionsSeq) return;
    users.value = u;
    images.value = imgs.filter((i) => i.status === 'active');
    networks.value = nets.filter((n) => n.project_name === item.project_name);
    gpuOptions.value = gpus;
    profiles.value = policies.flat();
  } catch (error) {
    if (mine === optionsSeq) errorToast(error);
  } finally {
    if (mine === optionsSeq) loading.value = false;
  }
}

watch(() => [open.value, props.node.id, props.item] as const, ([isOpen, , item]) => {
  ++optionsSeq;
  images.value = [];
  if (!isOpen || !item) return;
  prefill(item);
  void loadOptions(item);
}, { immediate: true });

const isContainer = computed(() => props.item?.instance_type === 'container');
const userOptions = computed(() =>
  users.value.map((u) => ({ label: u.email ? `${u.username}（${u.email}）` : u.username, value: u.id })),
);
const imageOptions = computed(() =>
  images.value
    .filter((i) => i.architecture === props.item?.architecture)
    .filter((i) => form.value.gpu_count === 0 || i.gpu_ready)
    .map((i) => ({
      label: `${i.name || i.code} ${i.version} · ${i.os_name} ${i.os_release} · ${i.architecture}${i.gpu_ready ? ' · GPU' : ''}`,
      value: i.id,
    })),
);
const networkOptions = computed(() =>
  networks.value.map((n) => ({ label: `直连网络 #${n.id}（${enumLabel('networkMode', n.mode).text} · ${n.project_name}）`, value: n.id })),
);
const modelOptions = computed(() =>
  [...new Set(gpuOptions.value.map((g) => g.model))].map((m) => ({ label: m, value: m })),
);
const accessOptions = computed<Array<{ disabled: boolean; label: string; value: AccessMode }>>(() => {
  const modes = new Set(gpuOptions.value.filter((g) => g.model === form.value.gpu_model).map((g) => g.access_mode));
  return [
    { disabled: !modes.has('shared'), label: '共享', value: 'shared' },
    { disabled: !modes.has('exclusive'), label: '独占', value: 'exclusive' },
  ];
});
const actionOptions = CUSTOM_ACTIONS.map((a) => ({ label: CUSTOM_ACTION_NAMES[a] ?? a, value: a }));

const policyKinds = computed<PolicyKind[]>(() => [
  'pricing', 'credit', 'metrics',
  ...(isContainer.value ? ['gpu-idle', 'gpu-scheduling'] as PolicyKind[] : []),
  ...(isContainer.value && form.value.gpu_count > 0 ? ['gpu-pricing'] as PolicyKind[] : []),
]);
function policyOptions(kind: PolicyKind) {
  return profiles.value.filter((p) =>
    p.kind === kind && p.status === 'active' && !!p.effective_at && Date.parse(p.effective_at) <= Date.now() &&
    (kind !== 'gpu-pricing' || (p.config.gpu_model === form.value.gpu_model && p.config.access_mode === form.value.access_mode)),
  ).map((p) => ({ label: `${p.code} v${p.version}`, value: p.id }));
}
function policyRules(kind: PolicyKind) {
  return [{ required: true, validator: async (_rule: unknown, value: unknown) => {
    if (!policyOptions(kind).some((p) => p.value === value)) throw new Error('请选择已发布、已生效且兼容的策略');
  } }];
}
function selectedPolicies(): PolicyVersions | null {
  const result: PolicyVersions = {};
  for (const kind of policyKinds.value) {
    const id = form.value.policy_versions[kind];
    if (!id || !policyOptions(kind).some((p) => p.value === id)) return null;
    result[kind] = id;
  }
  return result;
}

watch(
  () => form.value.gpu_count,
  (count) => {
    if (count === 0) form.value.gpu_model = undefined;
    const img = images.value.find((i) => i.id === form.value.image_id);
    if (count > 0 && img && !img.gpu_ready) form.value.image_id = undefined;
  },
);
watch(
  () => form.value.gpu_model,
  () => {
    const first = accessOptions.value.find((o) => !o.disabled);
    if (first && accessOptions.value.some((o) => o.value === form.value.access_mode && o.disabled)) {
      form.value.access_mode = first.value;
    }
  },
);

const required = (message: string) => [{ message, required: true }];
const ptRules = [
  { message: '请填写单价上限', required: true },
  { message: '非负数，最多 6 位小数', pattern: PT_PATTERN },
];
const positive = (label: string, integer: boolean) => [
  {
    required: true,
    validator: async (_r: unknown, v: unknown) => {
      if (typeof v !== 'number' || v <= 0) throw new Error(`${label}必须大于 0`);
      if (integer && !Number.isInteger(v)) throw new Error(`${label}必须是整数`);
      if (!integer) gibToBytes(v);
    },
  },
];

const confirmOpen = ref(false);
async function openConfirm() {
  try {
    await formRef.value?.validate();
  } catch {
    return;
  }
  confirmOpen.value = true;
}

function approvedConfig(item: UnmanagedView): CustomConfig | null {
  const f = form.value;
  if (!f.image_id || !f.network_option_id || loading.value || !images.value.some((i) => i.id === f.image_id && i.architecture === item.architecture)) return null;
  if (typeof f.vcpus !== 'number' || typeof f.memory_gib !== 'number' || typeof f.disk_gib !== 'number') return null;
  const memory =
    nativeMemoryBytes.value && f.memory_gib === bytesToGib(nativeMemoryBytes.value)
      ? nativeMemoryBytes.value
      : gibToBytes(f.memory_gib);
  return {
    allowed_user_actions: f.allowed_user_actions,
    gpu: {
      access_mode: f.access_mode,
      count: f.gpu_count,
      max_hourly_pt: f.max_hourly_pt.trim(),
      model: f.gpu_count > 0 ? (f.gpu_model ?? null) : null,
    },
    gpu_user_permissions: { ...f.gpu_user_permissions },
    image_id: f.image_id,
    init_parameters: {},
    instance_type: item.instance_type,
    network_option_id: f.network_option_id,
    node_id: props.node.id,
    resources: { disk_bytes: gibToBytes(f.disk_gib), memory_bytes: memory, vcpus: f.vcpus },
  };
}

async function submit() {
  const item = props.item;
  const owner = form.value.owner_user_id;
  if (!item?.incus_uuid || !owner) return false;
  const config = approvedConfig(item);
  const policyVersions = selectedPolicies();
  if (!policyVersions) return false;
  if (!config) return false;
  const uuid = item.incus_uuid;
  const r = await action.run(
    (key) =>
      nodesApi.importInstance(
        props.node.id,
        { approved_config: config, incus_uuid: uuid, owner_user_id: owner, policy_versions: policyVersions },
        { ifMatch: props.etag, key },
      ),
    { success: '已提交纳管' },
  );
  if (!r) return false;
  trackAccepted(r.data, `纳管 ${item.incus_name}`, () => emit('imported'));
  open.value = false;
  emit('imported');
  return true;
}
</script>

<template>
  <Drawer v-model:open="open" destroy-on-close :title="`纳管实例 ${item?.incus_name ?? ''}`" :width="720">
    <template v-if="item">
      <Alert
        class="mb-3"
        message="纳管后为自定义实例、免额度，不回溯计费。规格、类型、GPU 数量必须与节点上的实际配置一致，否则纳管任务失败。"
        show-icon
        type="info"
      />
      <Descriptions bordered class="mb-3" :column="2" size="small">
        <DescriptionsItem label="Incus 名称"><span class="font-mono">{{ item.incus_name }}</span></DescriptionsItem>
        <DescriptionsItem label="项目"><span class="font-mono">{{ item.project_name }}</span></DescriptionsItem>
        <DescriptionsItem label="Incus UUID" :span="2"><span class="font-mono text-xs">{{ item.incus_uuid }}</span></DescriptionsItem>
        <DescriptionsItem label="类型">{{ enumLabel('instanceType', item.instance_type).text }}</DescriptionsItem>
        <DescriptionsItem label="架构"><span class="font-mono">{{ item.architecture || '—' }}</span></DescriptionsItem>
        <DescriptionsItem label="原生配置" :span="2">
          <pre class="m-0 whitespace-pre-wrap font-mono text-xs">{{ JSON.stringify(item.config, null, 2) }}</pre>
        </DescriptionsItem>
      </Descriptions>

      <Form ref="formRef" layout="vertical" :model="form">
        <FormItem
          v-for="kind in policyKinds" :key="kind" :label="`策略 · ${kind}`"
          :name="['policy_versions', kind]" :rules="policyRules(kind)"
        >
          <Select
            v-model:value="form.policy_versions[kind]" :loading="loading" :options="policyOptions(kind)"
            placeholder="请选择已发布且已生效的兼容策略" not-found-content="没有可用策略，请先发布"
            show-search option-filter-prop="label"
          />
        </FormItem>
        <Row :gutter="12">
          <Col :span="12">
            <FormItem label="所有者" name="owner_user_id" :rules="required('请选择所有者')">
              <Select
                v-model:value="form.owner_user_id"
                :loading="loading"
                option-filter-prop="label"
                :options="userOptions"
                placeholder="仅正常状态的用户"
                show-search
              />
            </FormItem>
          </Col>
          <Col :span="12">
            <FormItem label="实例类型" extra="固定为节点上的实际类型">
              <Input disabled :value="enumLabel('instanceType', item.instance_type).text" />
            </FormItem>
          </Col>
          <Col :span="12">
            <FormItem label="镜像" name="image_id" :rules="required('请选择镜像')">
              <Select
                v-model:value="form.image_id"
                :loading="loading"
                option-filter-prop="label"
                :options="imageOptions"
                not-found-content="本节点、本项目没有已就绪的匹配镜像，请先在产品目录中准备"
                placeholder="本节点、本项目已就绪且架构一致的镜像"
                show-search
              />
            </FormItem>
          </Col>
          <Col :span="12">
            <FormItem label="网络" name="network_option_id" :rules="required('请选择网络')">
              <Select
                v-model:value="form.network_option_id"
                :loading="loading"
                not-found-content="本节点没有已启用且已验证的网络绑定"
                :options="networkOptions"
                placeholder="本节点已启用的网络绑定"
              />
            </FormItem>
          </Col>
        </Row>

        <Divider class="my-2" orientation="left" plain>规格</Divider>
        <Row :gutter="12">
          <Col :span="8">
            <FormItem label="vCPU" name="vcpus" :rules="positive('vCPU', true)">
              <InputNumber v-model:value="form.vcpus" class="w-full" :min="1" :precision="0" />
            </FormItem>
          </Col>
          <Col :span="8">
            <FormItem label="内存 (GiB)" name="memory_gib" :rules="positive('内存', false)">
              <InputNumber v-model:value="form.memory_gib" class="w-full" :min="0" />
            </FormItem>
          </Col>
          <Col :span="8">
            <FormItem extra="填写根磁盘实际大小" label="磁盘 (GiB)" name="disk_gib" :rules="positive('磁盘', false)">
              <InputNumber v-model:value="form.disk_gib" class="w-full" :min="0" />
            </FormItem>
          </Col>
        </Row>

        <template v-if="isContainer">
          <Divider class="my-2" orientation="left" plain>GPU</Divider>
          <Row :gutter="12">
            <Col :span="6">
              <FormItem label="数量" name="gpu_count" :rules="[{ message: '请填写 GPU 数量', required: true, type: 'number' }]">
                <InputNumber v-model:value="form.gpu_count" class="w-full" :max="64" :min="0" :precision="0" />
              </FormItem>
            </Col>
            <Col :span="10">
              <FormItem
                v-if="form.gpu_count > 0"
                label="型号"
                name="gpu_model"
                :rules="required('请选择 GPU 型号')"
              >
                <Select v-model:value="form.gpu_model" :options="modelOptions" placeholder="本节点的 GPU 型号" />
              </FormItem>
            </Col>
            <Col :span="8">
              <FormItem v-if="form.gpu_count > 0" label="访问模式" name="access_mode">
                <Select v-model:value="form.access_mode" :options="accessOptions" />
              </FormItem>
            </Col>
            <Col v-if="form.gpu_count > 0" :span="12">
              <FormItem label="单价上限 (pt/小时)" name="max_hourly_pt" :rules="ptRules">
                <Input v-model:value="form.max_hourly_pt" class="font-mono" />
              </FormItem>
            </Col>
          </Row>
        </template>

        <Divider class="my-2" orientation="left" plain>用户权限</Divider>
        <FormItem label="允许用户执行的操作">
          <CheckboxGroup v-model:value="form.allowed_user_actions" :options="actionOptions" />
        </FormItem>
        <FormItem label="用户可修改的 GPU 策略项">
          <div class="flex flex-wrap gap-x-6 gap-y-2">
            <label v-for="p in GPU_USER_PERMISSIONS" :key="p" class="flex items-center gap-2 text-sm">
              <Switch v-model:checked="form.gpu_user_permissions[p]" size="small" />
              {{ GPU_PERMISSION_NAMES[p] ?? p }}
            </label>
          </div>
        </FormItem>
        <div class="text-muted-foreground text-xs">初始化参数固定为 {}。</div>
      </Form>
    </template>
    <template #footer>
      <div class="flex justify-end gap-2">
        <Button @click="open = false">取消</Button>
        <Button :disabled="loading" :loading="action.pending.value" type="primary" @click="openConfirm">纳管</Button>
      </div>
    </template>
    <ConfirmAction
      v-model:open="confirmOpen"
      description="后台任务会核对节点上的原生配置并占用 IP、GPU 资源。"
      :on-confirm="submit"
      title="确认纳管"
    />
  </Drawer>
</template>
