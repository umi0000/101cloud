<script lang="ts" setup>
/**
 * approved_config 编辑表单（frontend/前端设计.md 7.4），用于管理员直接下发自定义实例。
 * props.initial 为初始值；表单合法时 emit('update:value', config)，否则 emit null。
 */
import type {
  AccessMode,
  CreateLogin,
  CustomAction,
  CustomConfig,
  GPUOption,
  Image,
  InitTemplate,
  InstanceType,
  NetworkOption,
  Node,
  NodeGPU,
  PolicyKind,
  PolicyVersion,
  PolicyVersions,
} from '#/api/types';

import { computed, onBeforeUnmount, onMounted, reactive, ref, shallowRef, watch } from 'vue';

import {
  Alert,
  CheckboxGroup,
  Col,
  Form,
  FormItem,
  Input,
  InputNumber,
  InputPassword,
  RadioButton,
  RadioGroup,
  Row,
  Select,
  Switch,
  Textarea,
} from 'ant-design-vue';

import { catalogApi, listAll, nodesApi, policiesApi } from '#/api/backend';
import { CUSTOM_ACTIONS, GPU_USER_PERMISSIONS, POLICY_KINDS } from '#/api/types';
import { errorToast } from '#/composables/api';
import { CUSTOM_ACTION_NAMES, enumLabel, GPU_PERMISSION_NAMES, POLICY_KIND_NAMES } from '#/locales/enums';
import { bytesToGib, formatMib, formatPt, gibToBytes, PT_PATTERN } from '#/utils/format';
import { emptyLoginDraft, imageSupportsLogin, validateLoginDraft } from '#/utils/instance-login';

import { ACCESS_MODE_NAMES } from './shared';

const props = defineProps<{ disabled?: boolean; initial?: CustomConfig | null }>();
const emit = defineEmits<{
  'update:value': [value: CustomConfig | null];
  'update:login': [value: CreateLogin | null];
  'update:policyVersions': [value: PolicyVersions];
}>();

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
});

const images = shallowRef<Image[]>([]);
const templates = shallowRef<InitTemplate[]>([]);
const networks = shallowRef<NetworkOption[]>([]);
const gpuOptions = shallowRef<GPUOption[]>([]);
const nodes = shallowRef<Node[]>([]);
const nodeGPUs = shallowRef<NodeGPU[]>([]);
const loading = ref(false);
const profiles = shallowRef<PolicyVersion[]>([]);
const policy = reactive<PolicyVersions>({});
const requiredPolicies = computed<PolicyKind[]>(() => [
  'pricing', 'credit', 'metrics',
  ...(form.instance_type === 'container' ? ['gpu-idle' as const, 'gpu-scheduling' as const] : []),
  ...(form.gpu_count > 0 ? ['gpu-pricing' as const] : []),
]);
function policyUsable(kind: PolicyKind, p: PolicyVersion): boolean {
  return p.kind === kind && p.status === 'active' && (!p.effective_at || Date.parse(p.effective_at) <= Date.now())
    && (kind !== 'gpu-pricing' || (p.config.gpu_model === form.gpu_model && p.config.access_mode === form.access_mode));
}
function policyOptions(kind: PolicyKind) {
  return profiles.value.filter((p) => policyUsable(kind, p)).map((p) => ({
    label: `${p.code} v${p.version}${kind === 'gpu-pricing' ? ` · ${formatPt(String(p.config.gpu_hourly_pt))} pt/h` : ''}`,
    value: p.id,
  }));
}
const selectedPolicies = computed<PolicyVersions>(() => Object.fromEntries(requiredPolicies.value.map((kind) => [kind, policy[kind]])));
watch(selectedPolicies, (v) => emit('update:policyVersions', { ...v }), { immediate: true });
const imagesLoading = ref(false);
const imageProject = computed(() => form.network_option_id
  ? networks.value.find((n) => n.id === form.network_option_id)?.project_name
  : nodes.value.find((n) => n.id === form.node_id)?.defaults.project_name);

// Component memory only: never merge credentials into approved_config or template parameters.
const loginDraft = ref(emptyLoginDraft());
const loginError = ref('请设置 root 密码。');
const loginValidating = ref(false);
const login = shallowRef<CreateLogin | null>(null);
let loginSequence = 0;
let disposed = false;
watch(() => loginDraft.value.mode, (mode) => {
  if (mode === 'password') loginDraft.value.sshPublicKeyText = '';
  else {
    loginDraft.value.rootPassword = '';
    loginDraft.value.rootPasswordConfirmation = '';
  }
}, { flush: 'sync' });
watch(loginDraft, async (draft) => {
  const sequence = ++loginSequence;
  login.value = null;
  loginValidating.value = true;
  const result = await validateLoginDraft({ ...draft });
  if (disposed || sequence !== loginSequence) return;
  loginError.value = result.error;
  login.value = result.login ?? null;
  loginValidating.value = false;
}, { deep: true, immediate: true, flush: 'sync' });
const loginImageError = computed(() => {
  const image = images.value.find((item) => item.id === form.image_id);
  const network = networks.value.find((item) => item.id === form.network_option_id && item.node_id === form.node_id);
  return image && network && !imageSupportsLogin(image, network)
    ? '所选节点 / 项目的镜像不支持 cloud-init，请更换镜像。' : '';
});
watch([login, loginImageError, imagesLoading], () => {
  emit('update:login', !loginImageError.value && !imagesLoading.value ? login.value : null);
}, { immediate: true, flush: 'sync' });
onBeforeUnmount(() => {
  disposed = true;
  ++loginSequence;
  loginDraft.value = emptyLoginDraft();
  login.value = null;
  emit('update:login', null);
});

onMounted(async () => {
  loading.value = true;
  try {
    const [n, t, p] = await Promise.all([
      listAll(nodesApi.list),
      listAll(catalogApi.templates),
      Promise.all(POLICY_KINDS.map((kind) => listAll((q) => policiesApi.list(kind, q)))),
    ]);
    nodes.value = n;
    templates.value = t.filter((x) => x.status === 'active');
    profiles.value = p.flat();
  } catch (error) {
    errorToast(error);
  } finally {
    loading.value = false;
  }
});

let imageSeq = 0;
watch(
  () => [form.node_id, form.instance_type, imageProject.value] as const,
  async ([node, instanceType, projectName], previous) => {
    const mine = ++imageSeq;
    images.value = [];
    if (previous && (previous[0] !== node || previous[1] !== instanceType || (previous[2] !== undefined && previous[2] !== projectName))) form.image_id = undefined;
    imagesLoading.value = false;
    if (!node || !projectName) return;
    imagesLoading.value = true;
    try {
      const available = await listAll(catalogApi.images, { instance_type: instanceType, node_id: node, project_name: projectName });
      if (mine !== imageSeq) return;
      images.value = available.filter((i) => i.status === 'active');
      if (form.image_id && !images.value.some((i) => i.id === form.image_id)) form.image_id = undefined;
    } catch (error) {
      if (mine === imageSeq) errorToast(error);
    } finally {
      if (mine === imageSeq) imagesLoading.value = false;
    }
  },
  { immediate: true },
);

/* ---------- 节点 → GPU 选项、节点 GPU 列表（fixed_uuids）、网络 ---------- */

let nodeSeq = 0;
watch(
  () => form.node_id,
  async (node, old) => {
    const mine = ++nodeSeq;
    if (old !== undefined && node !== old) form.network_option_id = undefined;
    gpuOptions.value = [];
    nodeGPUs.value = [];
    if (!node) return;
    try {
      const [o, g] = await Promise.all([
        listAll(catalogApi.gpuOptions, { node_id: node }),
        listAll((q) => nodesApi.gpus(node, q)),
      ]);
      if (mine !== nodeSeq) return;
      gpuOptions.value = o;
      nodeGPUs.value = g;
    } catch (error) {
      errorToast(error);
    }
  },
  { immediate: true },
);

let netSeq = 0;
watch(
  () => [form.node_id, form.instance_type] as const,
  async ([node, type]) => {
    const mine = ++netSeq;
    networks.value = [];
    if (!node) {
      form.network_option_id = undefined;
      return;
    }
    try {
      const n = await listAll(catalogApi.networkOptions, { instance_type: type, node_id: node });
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

watch(
  () => [form.node_id, form.gpu_model, form.gpu_count] as const,
  () => {
    form.fixed_uuids = [];
  },
);

/* ---------- 选项 ---------- */

const imageOptions = computed(() =>
  images.value
    .filter((i) => i.instance_type === form.instance_type)
    .map((i) => ({
      disabled: form.gpu_count > 0 && !i.gpu_ready,
      label: `${i.name}（${i.os_name} ${i.os_release} · ${i.architecture}${i.gpu_ready ? '，支持 GPU' : ''}）`,
      value: i.id,
    })),
);

const networkOptions = computed(() =>
  networks.value.map((n) => ({ label: `直连网络 #${n.id}（${enumLabel('networkMode', n.mode).text} · ${n.project_name}）`, value: n.id })),
);

/** 只有「启用」且未离线的节点可选；其余标注状态并禁用。 */
const nodeOptions = computed(() =>
  nodes.value.map((n) => {
    const usable = n.administrative_state === 'active' && n.health !== 'offline';
    return {
      disabled: !usable,
      label: usable
        ? n.name
        : `${n.name}（${enumLabel('adminState', n.administrative_state).text}，${enumLabel('health', n.health).text}）`,
      value: n.id,
    };
  }),
);

const templateOptions = computed(() =>
  templates.value.map((t) => ({ label: `${t.name}（${t.code} v${t.version}）`, value: t.id })),
);

/** GPU 选项按 model + access_mode 去重；单价由显式选择的 GPU 定价配置档决定。 */
const gpuChoice = computed({
  get: () => (form.gpu_model ? `${form.gpu_model}\u0000${form.access_mode}` : undefined),
  set: (key: string | undefined) => {
    const opt = gpuOptions.value.find((o) => `${o.model}\u0000${o.access_mode}` === key);
    if (!opt) return;
    form.gpu_model = opt.model;
    form.access_mode = opt.access_mode;
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
      label: `${o.model} · ${ACCESS_MODE_NAMES[o.access_mode]} · 显存 ${formatMib(o.memory_mib)} · ${o.availability === 'available' ? '有货' : '暂无'}`,
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
  for (const kind of requiredPolicies.value) {
    if (!profiles.value.some((p) => p.id === policy[kind] && policyUsable(kind, p))) out.push(`请选择已生效且兼容的${POLICY_KIND_NAMES[kind]}配置档`);
  }
  if (!form.node_id) out.push('请选择节点');
  if (!form.image_id) out.push('请选择镜像');
  else if (imagesLoading.value || !images.value.some((i) => i.id === form.image_id)) out.push('所选节点的镜像尚未就绪，请重新选择');
  else if (form.gpu_count > 0 && images.value.some((i) => i.id === form.image_id && !i.gpu_ready)) {
    out.push('所选镜像不支持 GPU');
  }
  if (loginImageError.value) out.push(loginImageError.value);
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
  const { image_id: imageId, network_option_id: networkOptionId, node_id: nodeId } = form;
  const diskBytes = bytesOrNull(form.disk_gib);
  const memoryBytes = bytesOrNull(form.memory_gib);
  if (!imageId || !networkOptionId || !nodeId || !diskBytes || !memoryBytes) return null;
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
    node_id: nodeId,
    resources: {
      disk_bytes: diskBytes,
      memory_bytes: memoryBytes,
      vcpus: form.vcpus,
    },
  };
  if (count > 0 && form.fixed_uuids.length > 0) config.gpu.fixed_uuids = [...form.fixed_uuids];
  if (form.init_template_version_id) config.init_template_version_id = form.init_template_version_id;
  return config;
});

watch(value, (v) => emit('update:value', v), { immediate: true });
</script>

<template>
  <Form :disabled="disabled" layout="vertical">
    <Row :gutter="12">
      <Col :xs="24" :sm="12">
        <FormItem label="实例类型" required>
          <RadioGroup v-model:value="form.instance_type" button-style="solid">
            <RadioButton value="container">容器</RadioButton>
            <RadioButton value="virtual-machine">虚拟机</RadioButton>
          </RadioGroup>
        </FormItem>
      </Col>
      <Col :xs="24" :sm="12">
        <FormItem label="节点" required>
          <Select
            v-model:value="form.node_id"
            :loading="loading"
            option-filter-prop="label"
            :options="nodeOptions"
            placeholder="选择节点"
            show-search
          />
        </FormItem>
      </Col>
      <Col :xs="24" :sm="12">
        <FormItem label="镜像" required>
          <Select
            v-model:value="form.image_id"
            :loading="imagesLoading"
            :options="imageOptions"
            :disabled="disabled || !form.node_id"
            :not-found-content="form.node_id ? '该节点暂无就绪镜像，请在产品目录中准备镜像' : '请先选择节点'"
            placeholder="选择镜像"
            show-search
            option-filter-prop="label"
          />
        </FormItem>
      </Col>
      <Col :xs="24" :sm="12">
        <FormItem label="网络" required>
          <Select
            v-model:value="form.network_option_id"
            :disabled="disabled || !form.node_id"
            :not-found-content="form.node_id ? '该节点没有已验证的网络' : '先选择节点'"
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
            :disabled="disabled || form.instance_type === 'virtual-machine'"
            :min="0"
            :precision="0"
          />
        </FormItem>
      </Col>
      <template v-if="form.gpu_count > 0">
        <Col :xs="24" :sm="16">
          <FormItem label="GPU 型号与共享方式" required>
            <Select
              v-model:value="gpuChoice"
              :disabled="disabled || !form.node_id"
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
        <Col :xs="24" :sm="16">
          <FormItem label="指定 GPU（可选）" :help="`不填由后端选最低负载卡；填写时数量必须为 ${form.gpu_count}`">
            <Select
              v-model:value="form.fixed_uuids"
              class="font-mono"
              :disabled="disabled || !form.node_id"
              :max-count="form.gpu_count"
              mode="multiple"
              :options="uuidOptions"
              placeholder="自动选择"
            />
          </FormItem>
        </Col>
      </template>
      <Col v-for="kind in requiredPolicies" :key="kind" :xs="24" :sm="12">
        <FormItem :label="POLICY_KIND_NAMES[kind]" required>
          <Select v-model:value="policy[kind]" :loading="loading" :options="policyOptions(kind)" placeholder="显式选择已生效配置档" />
        </FormItem>
      </Col>
      <Col :span="24">
        <FormItem label="登录方式" required>
          <RadioGroup id="custom-login-mode" v-model:value="loginDraft.mode" aria-label="登录方式">
            <RadioButton value="password">root 密码</RadioButton>
            <RadioButton value="ssh-key">SSH 公钥</RadioButton>
          </RadioGroup>
        </FormItem>
        <template v-if="loginDraft.mode === 'password'">
          <FormItem label="root 密码" html-for="custom-root-password" required>
            <InputPassword id="custom-root-password" v-model:value="loginDraft.rootPassword" autocomplete="new-password" aria-label="root 密码" aria-describedby="custom-password-help custom-login-error" />
          </FormItem>
          <FormItem label="确认 root 密码" html-for="custom-root-password-confirmation" required>
            <InputPassword id="custom-root-password-confirmation" v-model:value="loginDraft.rootPasswordConfirmation" autocomplete="new-password" aria-label="确认 root 密码" aria-describedby="custom-login-error" />
          </FormItem>
          <p id="custom-password-help" class="text-muted-foreground text-xs">8–128 个字符，不含控制字符；空格原样保留。</p>
        </template>
        <template v-else>
          <FormItem label="SSH 公钥" html-for="custom-ssh-public-keys" required>
            <Textarea id="custom-ssh-public-keys" v-model:value="loginDraft.sshPublicKeyText" :auto-size="{ minRows: 3, maxRows: 8 }" class="font-mono text-xs" autocomplete="off" :spellcheck="false" placeholder="ssh-ed25519 AAAA…" aria-label="SSH 公钥" aria-describedby="custom-ssh-keys-help custom-login-error" />
          </FormItem>
          <p id="custom-ssh-keys-help" class="text-muted-foreground text-xs">每行一条，1–10 条，每条最多 8192 字节；支持 Ed25519、RSA ≥2048 位、ECDSA nistp256/384/521。</p>
        </template>
        <p v-if="loginValidating" class="text-muted-foreground text-xs" role="status">正在校验登录凭据…</p>
        <p v-else-if="loginError" id="custom-login-error" class="text-destructive text-xs" role="alert">{{ loginError }}</p>
        <p class="text-muted-foreground text-xs">登录凭据通过所选节点 / 项目镜像的 cloud-init 自动配置，不需要选择初始化模板。SSH 服务需自行安装。</p>
      </Col>
      <Col :span="24">
        <FormItem label="初始化模板（可选，与登录配置独立）" help="仅用于额外初始化脚本；不要在初始化参数中填写密码或 SSH 公钥。">
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
            <pre class="overflow-x-auto font-mono">{{ JSON.stringify(selectedTemplate.parameter_schema, null, 2) }}</pre>
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
