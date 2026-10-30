<script lang="ts" setup>
/**
 * approved_config 编辑表单（frontend/前端设计.md 7.5），用于管理员直接下发自定义实例，可复用于审批与纳管。
 * props.initial 为初始值；表单合法时 emit('update:value', config)，否则 emit null。
 */
import type {
  AccessMode,
  CustomAction,
  CustomConfig,
  GPUOption,
  Image,
  InitTemplate,
  InstanceType,
  NetworkOption,
  Node,
  NodeGPU,
  Zone,
} from '#/api/types';

import { computed, onMounted, reactive, ref, shallowRef, watch } from 'vue';

import {
  Alert,
  CheckboxGroup,
  Col,
  Form,
  FormItem,
  Input,
  InputNumber,
  RadioButton,
  RadioGroup,
  Row,
  Select,
  Switch,
  Textarea,
} from 'ant-design-vue';

import { catalogApi, listAll, nodesApi } from '#/api/backend';
import { CUSTOM_ACTIONS, GPU_USER_PERMISSIONS } from '#/api/types';
import { errorToast } from '#/composables/api';
import { CUSTOM_ACTION_NAMES, enumLabel, GPU_PERMISSION_NAMES } from '#/locales/enums';
import { bytesToGib, formatMib, formatPt, gibToBytes, PT_PATTERN } from '#/utils/format';

import { ACCESS_MODE_NAMES } from './shared';

const props = defineProps<{ initial?: CustomConfig | null }>();
const emit = defineEmits<{ 'update:value': [value: CustomConfig | null] }>();

interface FormState {
  access_mode: AccessMode;
  allowed_user_actions: CustomAction[];
  disk_gib: number;
  fixed_uuids: string[];
  gpu_count: number;
  gpu_model: string | undefined;
  gpu_user_permissions: Record<string, boolean>;
  image_id: string | undefined;
  init_parameters: string;
  init_template_version_id: string | undefined;
  instance_type: InstanceType;
  max_hourly_pt: string;
  memory_gib: number;
  network_option_id: string | undefined;
  node_id: string | undefined;
  vcpus: number;
  zone_id: string | undefined;
}

const init = props.initial;
const form = reactive<FormState>({
  access_mode: init?.gpu.access_mode ?? 'shared',
  allowed_user_actions: [...(init?.allowed_user_actions ?? ['start', 'stop', 'restart', 'console'])],
  disk_gib: init ? bytesToGib(init.resources.disk_bytes) : 40,
  fixed_uuids: [...(init?.gpu.fixed_uuids ?? [])],
  gpu_count: init?.gpu.count ?? 0,
  gpu_model: init?.gpu.model ?? undefined,
  gpu_user_permissions: Object.fromEntries(
    GPU_USER_PERMISSIONS.map((k) => [k, init?.gpu_user_permissions?.[k] ?? false]),
  ),
  image_id: init?.image_id,
  init_parameters: JSON.stringify(init?.init_parameters ?? {}, null, 2),
  init_template_version_id: init?.init_template_version_id,
  instance_type: init?.instance_type ?? 'container',
  max_hourly_pt: init?.gpu.max_hourly_pt ?? '0',
  memory_gib: init ? bytesToGib(init.resources.memory_bytes) : 4,
  network_option_id: init?.network_option_id,
  node_id: init?.node_id,
  vcpus: init?.resources.vcpus ?? 2,
  zone_id: init?.zone_id,
});

const zones = shallowRef<Zone[]>([]);
const images = shallowRef<Image[]>([]);
const templates = shallowRef<InitTemplate[]>([]);
const networks = shallowRef<NetworkOption[]>([]);
const gpuOptions = shallowRef<GPUOption[]>([]);
const nodes = shallowRef<Node[]>([]);
const nodeGPUs = shallowRef<NodeGPU[]>([]);
const loading = ref(false);

onMounted(async () => {
  loading.value = true;
  try {
    const [z, i, t] = await Promise.all([
      listAll(catalogApi.zones),
      listAll(catalogApi.images),
      listAll(catalogApi.templates),
    ]);
    zones.value = z.filter((x) => x.status === 'active');
    images.value = i.filter((x) => x.status === 'active');
    templates.value = t.filter((x) => x.status === 'active');
  } catch (error) {
    errorToast(error);
  } finally {
    loading.value = false;
  }
});

/* ---------- 可用区 → 节点、GPU 选项、网络 ---------- */

let zoneSeq = 0;
watch(
  () => form.zone_id,
  async (zone, old) => {
    const mine = ++zoneSeq;
    if (old !== undefined && zone !== old) form.node_id = undefined;
    if (!zone) {
      nodes.value = [];
      gpuOptions.value = [];
      return;
    }
    try {
      const [n, g] = await Promise.all([
        listAll(nodesApi.list, { zone_id: zone }),
        listAll(catalogApi.gpuOptions, { zone_id: zone }),
      ]);
      if (mine !== zoneSeq) return;
      nodes.value = n;
      gpuOptions.value = g;
    } catch (error) {
      errorToast(error);
    }
  },
  { immediate: true },
);

let netSeq = 0;
watch(
  () => [form.zone_id, form.instance_type] as const,
  async ([zone, type]) => {
    const mine = ++netSeq;
    if (!zone) {
      networks.value = [];
      form.network_option_id = undefined;
      return;
    }
    try {
      const n = await listAll(catalogApi.networkOptions, { instance_type: type, zone_id: zone });
      if (mine !== netSeq) return;
      networks.value = n;
      if (form.network_option_id && !n.some((x) => x.id === form.network_option_id)) {
        form.network_option_id = undefined;
      }
    } catch (error) {
      errorToast(error);
    }
  },
  { immediate: true },
);

watch(
  () => form.instance_type,
  (type) => {
    if (type === 'virtual-machine') form.gpu_count = 0;
    const img = images.value.find((x) => x.id === form.image_id);
    if (img && img.instance_type !== type) form.image_id = undefined;
  },
);

/* ---------- 指定节点 → 该节点 GPU 列表（fixed_uuids） ---------- */

let gpuSeq = 0;
watch(
  () => form.node_id,
  async (node) => {
    const mine = ++gpuSeq;
    nodeGPUs.value = [];
    if (!node) return;
    try {
      const g = await listAll((q) => nodesApi.gpus(node, q));
      if (mine === gpuSeq) nodeGPUs.value = g;
    } catch (error) {
      errorToast(error);
    }
  },
  { immediate: true },
);
watch(
  () => [form.node_id, form.gpu_model, form.gpu_count] as const,
  () => {
    form.fixed_uuids = [];
  },
);

/* ---------- 选项 ---------- */

const zoneOptions = computed(() => zones.value.map((z) => ({ label: `${z.name}（${z.code}）`, value: z.id })));

const imageOptions = computed(() =>
  images.value
    .filter((i) => i.instance_type === form.instance_type)
    .map((i) => ({
      disabled: form.gpu_count > 0 && !i.gpu_ready,
      label: `${i.name}（${i.os_name} ${i.os_release}${i.gpu_ready ? '，支持 GPU' : ''}）`,
      value: i.id,
    })),
);

const networkOptions = computed(() =>
  networks.value.map((n) => ({ label: `直连网络 #${n.id}（${enumLabel('networkMode', n.mode).text}）`, value: n.id })),
);

const nodeOptions = computed(() =>
  nodes.value.map((n) => ({
    label: `${n.name}（${enumLabel('health', n.health).text}，${enumLabel('adminState', n.administrative_state).text}）`,
    value: n.id,
  })),
);

const templateOptions = computed(() =>
  templates.value.map((t) => ({ label: `${t.name}（${t.code} v${t.version}）`, value: t.id })),
);

/** GPU 选项按 model + access_mode 去重；选中时同时设置 access_mode 与默认单价上限。 */
const gpuChoice = computed({
  get: () => (form.gpu_model ? `${form.gpu_model}\u0000${form.access_mode}` : undefined),
  set: (key: string | undefined) => {
    const opt = gpuOptions.value.find((o) => `${o.model}\u0000${o.access_mode}` === key);
    if (!opt) return;
    form.gpu_model = opt.model;
    form.access_mode = opt.access_mode;
    form.max_hourly_pt = opt.gpu_hourly_pt;
  },
});
const gpuChoiceOptions = computed(() => {
  const seen = new Set<string>();
  const out: Array<{ label: string; value: string }> = [];
  for (const o of gpuOptions.value) {
    const key = `${o.model}\u0000${o.access_mode}`;
    if (seen.has(key)) continue;
    seen.add(key);
    out.push({
      label: `${o.model} · ${ACCESS_MODE_NAMES[o.access_mode]} · ${formatPt(o.gpu_hourly_pt)} pt/h · 显存 ${formatMib(o.memory_mib)} · ${o.availability === 'available' ? '有货' : '暂无'}`,
      value: key,
    });
  }
  if (form.gpu_model && !seen.has(`${form.gpu_model}\u0000${form.access_mode}`)) {
    out.push({
      label: `${form.gpu_model} · ${ACCESS_MODE_NAMES[form.access_mode]}`,
      value: `${form.gpu_model}\u0000${form.access_mode}`,
    });
  }
  return out;
});

const uuidOptions = computed(() =>
  nodeGPUs.value
    .filter((g) => !form.gpu_model || g.model === form.gpu_model)
    .map((g) => ({
      disabled: g.status !== 'available',
      label: `${g.model} · ${g.uuid}（${enumLabel('gpuStatus', g.status).text}，已分配 ${g.allocation_count}/${g.max_allocations}）`,
      value: g.uuid,
    })),
);

const actionOptions = CUSTOM_ACTIONS.map((a) => ({ label: CUSTOM_ACTION_NAMES[a] ?? a, value: a }));

const selectedTemplate = computed(() => templates.value.find((t) => t.id === form.init_template_version_id));
watch(
  () => form.init_template_version_id,
  (id, old) => {
    if (!id && old) form.init_parameters = '{}';
  },
);

/* ---------- 校验与输出 ---------- */

const parsedParams = computed<null | Record<string, unknown>>(() => {
  try {
    const v: unknown = JSON.parse(form.init_parameters || '{}');
    return v !== null && typeof v === 'object' && !Array.isArray(v) ? (v as Record<string, unknown>) : null;
  } catch {
    return null;
  }
});

function bytesOrNull(gib: number): null | string {
  try {
    const b = gibToBytes(gib);
    return b === '0' ? null : b;
  } catch {
    return null;
  }
}

const problems = computed(() => {
  const out: string[] = [];
  if (!form.zone_id) out.push('请选择可用区');
  if (!form.image_id) out.push('请选择镜像');
  else if (form.gpu_count > 0 && images.value.some((i) => i.id === form.image_id && !i.gpu_ready)) {
    out.push('所选镜像不支持 GPU');
  }
  if (!form.network_option_id) out.push('请选择网络');
  if (!Number.isInteger(form.vcpus) || form.vcpus < 1 || form.vcpus > 65_535) out.push('vCPU 必须是 1–65535 的整数');
  if (!bytesOrNull(form.memory_gib)) out.push('内存必须大于 0（最多 3 位小数）');
  if (!bytesOrNull(form.disk_gib)) out.push('磁盘必须大于 0（最多 3 位小数）');
  if (!Number.isInteger(form.gpu_count) || form.gpu_count < 0) out.push('GPU 数量必须是非负整数');
  if (form.instance_type === 'virtual-machine' && form.gpu_count > 0) out.push('虚拟机不能挂载 GPU');
  if (form.gpu_count > 0 && !form.gpu_model) out.push('请选择 GPU 型号');
  if (!PT_PATTERN.test(form.max_hourly_pt)) out.push('GPU 单价上限必须是非负数，最多 6 位小数');
  if (form.fixed_uuids.length > 0 && form.fixed_uuids.length !== form.gpu_count) {
    out.push(`指定 GPU 数量必须等于 ${form.gpu_count}`);
  }
  if (!parsedParams.value) out.push('初始化参数必须是 JSON 对象');
  else if (!form.init_template_version_id && Object.keys(parsedParams.value).length > 0) {
    out.push('未选择初始化模板时初始化参数必须为空');
  }
  return out;
});

const value = computed<CustomConfig | null>(() => {
  if (problems.value.length > 0) return null;
  const { image_id: imageId, network_option_id: networkOptionId, zone_id: zoneId } = form;
  const diskBytes = bytesOrNull(form.disk_gib);
  const memoryBytes = bytesOrNull(form.memory_gib);
  if (!imageId || !networkOptionId || !zoneId || !diskBytes || !memoryBytes) return null;
  const count = form.gpu_count;
  const config: CustomConfig = {
    allowed_user_actions: [...form.allowed_user_actions],
    gpu: {
      access_mode: form.access_mode,
      count,
      max_hourly_pt: form.max_hourly_pt,
      model: count > 0 ? (form.gpu_model ?? null) : null,
    },
    gpu_user_permissions: { ...form.gpu_user_permissions },
    image_id: imageId,
    init_parameters: parsedParams.value ?? {},
    instance_type: form.instance_type,
    network_option_id: networkOptionId,
    resources: {
      disk_bytes: diskBytes,
      memory_bytes: memoryBytes,
      vcpus: form.vcpus,
    },
    zone_id: zoneId,
  };
  if (count > 0 && form.fixed_uuids.length > 0) config.gpu.fixed_uuids = [...form.fixed_uuids];
  if (form.node_id) config.node_id = form.node_id;
  if (form.init_template_version_id) config.init_template_version_id = form.init_template_version_id;
  return config;
});

watch(value, (v) => emit('update:value', v), { immediate: true });
</script>

<template>
  <Form layout="vertical">
    <Row :gutter="12">
      <Col :span="12">
        <FormItem label="实例类型" required>
          <RadioGroup v-model:value="form.instance_type" button-style="solid">
            <RadioButton value="container">容器</RadioButton>
            <RadioButton value="virtual-machine">虚拟机</RadioButton>
          </RadioGroup>
        </FormItem>
      </Col>
      <Col :span="12">
        <FormItem label="可用区" required>
          <Select v-model:value="form.zone_id" :loading="loading" :options="zoneOptions" placeholder="选择可用区" />
        </FormItem>
      </Col>
      <Col :span="12">
        <FormItem label="镜像" required>
          <Select
            v-model:value="form.image_id"
            :loading="loading"
            :options="imageOptions"
            placeholder="选择镜像"
            show-search
            option-filter-prop="label"
          />
        </FormItem>
      </Col>
      <Col :span="12">
        <FormItem label="网络" required>
          <Select
            v-model:value="form.network_option_id"
            :disabled="!form.zone_id"
            :not-found-content="form.zone_id ? '该可用区没有已验证的网络' : '先选择可用区'"
            :options="networkOptions"
            placeholder="选择网络"
          />
        </FormItem>
      </Col>
      <Col :span="8">
        <FormItem label="vCPU" required>
          <InputNumber v-model:value="form.vcpus" class="w-full" :max="65535" :min="1" :precision="0" />
        </FormItem>
      </Col>
      <Col :span="8">
        <FormItem label="内存（GiB）" required>
          <InputNumber v-model:value="form.memory_gib" class="w-full" :min="0.001" :precision="3" :step="1" />
        </FormItem>
      </Col>
      <Col :span="8">
        <FormItem label="磁盘（GiB）" required>
          <InputNumber v-model:value="form.disk_gib" class="w-full" :min="0.001" :precision="3" :step="10" />
        </FormItem>
      </Col>
      <Col :span="8">
        <FormItem label="GPU 数量" required>
          <InputNumber
            v-model:value="form.gpu_count"
            class="w-full"
            :disabled="form.instance_type === 'virtual-machine'"
            :min="0"
            :precision="0"
          />
        </FormItem>
      </Col>
      <template v-if="form.gpu_count > 0">
        <Col :span="16">
          <FormItem label="GPU 型号与共享方式" required>
            <Select
              v-model:value="gpuChoice"
              :disabled="!form.zone_id"
              :options="gpuChoiceOptions"
              placeholder="选择 GPU"
            />
          </FormItem>
        </Col>
        <Col :span="8">
          <FormItem label="GPU 单价上限（pt/h）" required>
            <Input v-model:value="form.max_hourly_pt" class="font-mono" />
          </FormItem>
        </Col>
        <Col :span="16">
          <FormItem label="指定 GPU（可选）" :help="form.node_id ? `不填由后端选最低负载卡；填写时数量必须为 ${form.gpu_count}` : '先指定节点才能选择具体 GPU'">
            <Select
              v-model:value="form.fixed_uuids"
              class="font-mono"
              :disabled="!form.node_id"
              :max-count="form.gpu_count"
              mode="multiple"
              :options="uuidOptions"
              placeholder="自动选择"
            />
          </FormItem>
        </Col>
      </template>
      <Col :span="12">
        <FormItem label="指定节点（可选）">
          <Select
            v-model:value="form.node_id"
            allow-clear
            :disabled="!form.zone_id"
            :options="nodeOptions"
            placeholder="自动调度"
          />
        </FormItem>
      </Col>
      <Col :span="12">
        <FormItem label="初始化模板（可选）">
          <Select
            v-model:value="form.init_template_version_id"
            allow-clear
            :loading="loading"
            :options="templateOptions"
            placeholder="不使用"
          />
        </FormItem>
      </Col>
      <Col v-if="form.init_template_version_id" :span="24">
        <FormItem label="初始化参数（JSON 对象）">
          <Textarea v-model:value="form.init_parameters" :auto-size="{ minRows: 3, maxRows: 10 }" class="font-mono" />
          <details v-if="selectedTemplate" class="mt-1 text-xs">
            <summary class="text-muted-foreground cursor-pointer">参数结构</summary>
            <pre class="font-mono">{{ JSON.stringify(selectedTemplate.parameter_schema, null, 2) }}</pre>
          </details>
        </FormItem>
      </Col>
      <Col :span="24">
        <FormItem label="允许用户执行的操作">
          <CheckboxGroup v-model:value="form.allowed_user_actions" :options="actionOptions" />
        </FormItem>
      </Col>
      <Col :span="24">
        <FormItem label="允许用户修改的 GPU 策略项">
          <div class="flex flex-wrap gap-x-6 gap-y-2">
            <label v-for="k in GPU_USER_PERMISSIONS" :key="k" class="flex items-center gap-2 text-sm">
              <Switch v-model:checked="form.gpu_user_permissions[k]" size="small" />
              {{ GPU_PERMISSION_NAMES[k] ?? k }}
            </label>
          </div>
        </FormItem>
      </Col>
    </Row>
    <Alert v-if="problems.length > 0" type="warning" show-icon :message="problems.join('；')" />
  </Form>
</template>
