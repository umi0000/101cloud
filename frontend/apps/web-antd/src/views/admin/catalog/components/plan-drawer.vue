<script lang="ts" setup>
import type { CatalogRefs } from '../refs';

import type {
  AccessMode,
  AllowedOverrides,
  CustomAction,
  GPUPolicyConfig,
  Image,
  InstanceType,
  OverrideKey,
  Plan,
  PlanInput,
  PolicyKind,
  PolicyVersion,
  PolicyVersions,
} from '#/api/types';

import { computed, ref, watch } from 'vue';

import {
  Alert,
  AutoComplete,
  Button,
  CheckboxGroup,
  Divider,
  Drawer,
  Form,
  FormItem,
  Input,
  InputNumber,
  RadioButton,
  RadioGroup,
  Select,
  Space,
  Switch,
} from 'ant-design-vue';

import { catalogApi } from '#/api/backend';
import { CUSTOM_ACTIONS, GPU_USER_PERMISSIONS } from '#/api/types';
import { errorToast, useAction, useResource } from '#/composables/api';
import { CUSTOM_ACTION_NAMES, enumLabel, ENUMS, GPU_PERMISSION_NAMES, POLICY_KIND_NAMES } from '#/locales/enums';
import { bytesToGib, formatPt, formatTime, gibToBytes, PT_PATTERN, ptToMicros } from '#/utils/format';

import { CODE_PATTERN } from './rules';

type PlanPolicyKind = PolicyKind;
type Mode = 'create' | 'edit' | 'view';

interface OverrideForm {
  enabled: boolean;
  max: number | undefined;
  min: number | undefined;
  step: number | undefined;
}

interface PlanForm {
  access_mode: AccessMode;
  allowed_user_actions: CustomAction[];
  code: string;
  disk_gib: number | undefined;
  gpu_count: number;
  gpu_model: string;
  gpu_policy: GPUPolicyConfig;
  gpu_user_permissions: Record<string, boolean>;
  image_ids: string[];
  init_template_version_id: string | undefined;
  instance_type: InstanceType;
  max_hourly_pt: string;
  memory_gib: number | undefined;
  name: string;
  network_option_ids: string[];
  node_ids: string[];
  overrides: Record<OverrideKey, OverrideForm>;
  policy: Record<PlanPolicyKind, string | undefined>;
  vcpus: number | undefined;
  version: string;
}

const props = defineProps<{ mode: Mode; refs: CatalogRefs; source: null | Plan }>();
const open = defineModel<boolean>('open', { default: false });
const emit = defineEmits<{ saved: [] }>();

const OVERRIDE_KEYS: OverrideKey[] = ['vcpus', 'memory_bytes', 'disk_bytes', 'gpu_count'];
const OVERRIDE_META: Record<OverrideKey, { gib: boolean; label: string; unit: string }> = {
  disk_bytes: { gib: true, label: '磁盘', unit: 'GiB' },
  gpu_count: { gib: false, label: 'GPU 数量', unit: '张' },
  memory_bytes: { gib: true, label: '内存', unit: 'GiB' },
  vcpus: { gib: false, label: 'vCPU', unit: '核' },
};

const typeOptions = Object.entries(ENUMS.instanceType ?? {}).map(([value, l]) => ({ label: l.text, value }));

const emptyOverride = (): OverrideForm => ({ enabled: false, max: undefined, min: undefined, step: undefined });

function emptyForm(): PlanForm {
  return {
    access_mode: 'exclusive',
    allowed_user_actions: ['start', 'stop', 'restart', 'console'],
    code: '',
    disk_gib: undefined,
    gpu_count: 0,
    gpu_model: '',
    gpu_policy: {
      idle: { action: 'detach_gpu', enabled: false, timeout_seconds: 1800 },
      rebalance: { allow_live: false, enabled: false, max_hourly_pt: '0' },
    },
    gpu_user_permissions: Object.fromEntries(GPU_USER_PERMISSIONS.map((key) => [key, false])),
    image_ids: [],
    init_template_version_id: undefined,
    instance_type: 'container',
    max_hourly_pt: '0',
    memory_gib: undefined,
    name: '',
    network_option_ids: [],
    node_ids: [],
    overrides: {
      disk_bytes: emptyOverride(),
      gpu_count: emptyOverride(),
      memory_bytes: emptyOverride(),
      vcpus: emptyOverride(),
    },
    policy: { credit: undefined, 'gpu-idle': undefined, 'gpu-pricing': undefined, 'gpu-scheduling': undefined, metrics: undefined, pricing: undefined },
    vcpus: undefined,
    version: '1',
  };
}

const fromBound = (key: OverrideKey, s: string) => (OVERRIDE_META[key].gib ? bytesToGib(s) : Number(s));
const toBound = (key: OverrideKey, n: number) => (OVERRIDE_META[key].gib ? gibToBytes(n) : String(n));

function formFromPlan(p: Plan, copy: boolean): PlanForm {
  const overrides = {} as Record<OverrideKey, OverrideForm>;
  for (const k of OVERRIDE_KEYS) {
    const b = p.allowed_overrides[k];
    overrides[k] = b
      ? { enabled: true, max: fromBound(k, b.max), min: fromBound(k, b.min), step: fromBound(k, b.step) }
      : emptyOverride();
  }
  return {
    access_mode: p.gpu.access_mode,
    allowed_user_actions: [...p.allowed_user_actions],
    code: copy ? '' : p.code,
    disk_gib: bytesToGib(p.resources.disk_bytes),
    gpu_count: p.gpu.count,
    gpu_model: p.gpu.model ?? '',
    gpu_policy: { idle: { ...p.gpu_policy.idle }, rebalance: { ...p.gpu_policy.rebalance } },
    gpu_user_permissions: Object.fromEntries(GPU_USER_PERMISSIONS.map((key) => [key, p.gpu_user_permissions[key] ?? false])),
    image_ids: [...p.image_ids],
    init_template_version_id: p.init_template_version_id ?? undefined,
    instance_type: p.instance_type,
    max_hourly_pt: formatPt(p.gpu.max_hourly_pt),
    memory_gib: bytesToGib(p.resources.memory_bytes),
    name: p.name,
    network_option_ids: [...p.network_option_ids],
    node_ids: [...p.node_ids],
    overrides,
    policy: {
      credit: p.policy_versions.credit,
      'gpu-idle': p.policy_versions['gpu-idle'],
      'gpu-pricing': p.policy_versions['gpu-pricing'],
      'gpu-scheduling': p.policy_versions['gpu-scheduling'],
      metrics: p.policy_versions.metrics,
      pricing: p.policy_versions.pricing,
    },
    vcpus: p.resources.vcpus,
    version: copy ? '1' : p.version,
  };
}

const form = ref<PlanForm>(emptyForm());
const tried = ref(false);
const action = useAction();
const detail = useResource(() => (props.source ? catalogApi.plan(props.source.id) : null));
const readonly = computed(() => props.mode === 'view');
const title = computed(() => {
  if (props.mode === 'create') return props.source ? `复制套餐 ${props.source.code}` : '新建套餐';
  const p = detail.data.value ?? props.source;
  return `${props.mode === 'edit' ? '编辑套餐' : '套餐'} ${p?.code ?? ''} v${p?.version ?? ''}`;
});

async function fill() {
  tried.value = false;
  if (props.mode === 'create') {
    form.value = props.source ? formFromPlan(props.source, true) : emptyForm();
    return;
  }
  try {
    await detail.reload();
    if (detail.data.value) form.value = formFromPlan(detail.data.value, false);
  } catch (error) {
    errorToast(error);
  }
}

watch(open, (v) => {
  if (v) void fill();
});
watch(form, () => action.resetKey(), { deep: true });

const isVM = computed(() => form.value.instance_type === 'virtual-machine');
const hasModel = computed(() => form.value.gpu_model.trim() !== '');
const requiredPolicies = computed<PlanPolicyKind[]>(() => isVM.value
  ? ['pricing', 'credit', 'metrics']
  : ['pricing', 'credit', 'metrics', 'gpu-idle', 'gpu-scheduling']);

// Dependent selections are pruned only after a user changes a selector, never
// while loading/cloning a saved plan or refreshing reference data.
function pruneImages() {
  form.value.image_ids = form.value.image_ids.filter((id) => imageOptions.value.some((o) => o.value === id && !o.disabled));
}

function onNodesChange() {
  form.value.network_option_ids = form.value.network_option_ids.filter((id) => networkOptions.value.some((o) => o.value === id && !o.disabled));
  pruneImages();
}

function onInstanceTypeChange() {
  const f = form.value;
  if (isVM.value) {
    f.gpu_count = 0;
    f.gpu_model = '';
    f.max_hourly_pt = '0';
    f.overrides.gpu_count = emptyOverride();
    f.gpu_policy.idle.enabled = false;
    f.gpu_policy.rebalance.enabled = false;
    f.gpu_policy.rebalance.allow_live = false;
    f.allowed_user_actions = f.allowed_user_actions.filter((a) => !a.startsWith('gpu_'));
    f.gpu_user_permissions = Object.fromEntries(GPU_USER_PERMISSIONS.map((key) => [key, false]));
  }
  onNodesChange();
}

function onGpuModelChange() {
  if (!hasModel.value) {
    form.value.gpu_count = 0;
    form.value.overrides.gpu_count = emptyOverride();
  }
  pruneImages();
}

function readyImageNodes(image: Image) {
  const f = form.value;
  const bindings = props.refs.bindings.value.filter((b) => f.network_option_ids.includes(b.id) && f.node_ids.includes(b.node_id));
  return image.nodes.filter((n) => n.status === 'ready' && f.node_ids.includes(n.node_id) && bindings.some((b) => b.node_id === n.node_id && b.project_name === n.project_name));
}

function compatibleImage(image: Image) {
  const f = form.value;
  const bindings = props.refs.bindings.value.filter((b) => f.network_option_ids.includes(b.id));
  const ready = readyImageNodes(image);
  return bindings.length > 0 && bindings.length === f.network_option_ids.length && bindings.every((b) =>
    ready.some((n) => n.node_id === b.node_id && n.project_name === b.project_name && (!hasModel.value || n.gpu_ready)),
  );
}

function imageNodeLabel(image: Image) {
  const ready = readyImageNodes(image);
  return ready.length ? ready.map((n) => `${props.refs.nodeName(n.node_id)}/${n.project_name} · ${n.architecture}${n.gpu_ready ? ' · GPU' : ''}`).join('；') : '所选网络节点暂无就绪配置';
}

const imageOptions = computed(() =>
  props.refs.images.value
    .filter((i) => i.instance_type === form.value.instance_type || form.value.image_ids.includes(i.id))
    .filter((i) => i.status === 'active' || form.value.image_ids.includes(i.id))
    .filter((i) => readyImageNodes(i).length > 0 || form.value.image_ids.includes(i.id))
    .map((i) => ({
      disabled: i.instance_type !== form.value.instance_type || !compatibleImage(i),
      label: `${i.name} · ${i.os_name} ${i.os_release} · ${imageNodeLabel(i)}${i.status === 'active' ? '' : ' · 未启用'}`,
      value: i.id,
    })),
);

const networkOptions = computed(() =>
  props.refs.bindings.value
    .filter((b) => b.status === 'active' || form.value.network_option_ids.includes(b.id))
    .filter((b) => form.value.node_ids.includes(b.node_id) || form.value.network_option_ids.includes(b.id))
    .filter((b) => !isVM.value || b.mode !== 'macvlan' || form.value.network_option_ids.includes(b.id))
    .map((b) => ({
      disabled: b.status !== 'active' || !form.value.node_ids.includes(b.node_id) || (isVM.value && b.mode === 'macvlan'),
      label: `${props.refs.bindingName(b.id)} · ${b.project_name}${b.status === 'active' ? '' : ' · 未启用'}${b.validation_status === 'verified' ? '' : `（${enumLabel('validation', b.validation_status).text}）`}`,
      value: b.id,
    })),
);

const nodeOptions = computed(() => props.refs.nodes.value.map((n) => ({
  label: `${n.name} · ${enumLabel('adminState', n.administrative_state).text} · ${enumLabel('health', n.health).text}`,
  value: n.id,
})));
const userActionOptions = computed(() => CUSTOM_ACTIONS.map((a) => ({
  disabled: isVM.value && a.startsWith('gpu_'),
  label: CUSTOM_ACTION_NAMES[a] ?? a,
  value: a,
})));

const idleConfig = computed(() => props.refs.policies['gpu-idle']?.find((p) => p.id === form.value.policy['gpu-idle'])?.config);
const idleActions = computed(() => {
  const actions = idleConfig.value?.allowed_actions;
  return Array.isArray(actions) ? actions.map((a) => a === 'release_gpu' ? 'detach_gpu' : String(a)) : [];
});
const idleActionOptions = computed(() => [
  { disabled: !!idleConfig.value && !idleActions.value.includes('detach_gpu'), label: '移除 GPU', value: 'detach_gpu' },
  { disabled: !!idleConfig.value && !idleActions.value.includes('stop_instance'), label: '停止实例', value: 'stop_instance' },
]);
const idleHint = computed(() => {
  const c = idleConfig.value;
  if (!c) return '请先选择 GPU 空闲政策；时长和动作即使关闭回收也须符合所选政策。';
  const actions = idleActionOptions.value.filter((a) => !a.disabled).map((a) => a.label).join('、') || '无';
  return `所选政策：${c.enabled ? '允许' : '未启用'}空闲回收；${String(c.min_timeout_seconds)}–${String(c.max_timeout_seconds)} 秒，默认 ${String(c.default_timeout_seconds)} 秒；允许动作：${actions}。关闭回收时仍校验时长和动作。`;
});

function applyIdleDefaults() {
  const c = idleConfig.value;
  const first = idleActions.value[0];
  if (typeof c?.default_timeout_seconds === 'number') form.value.gpu_policy.idle.timeout_seconds = c.default_timeout_seconds;
  if (first === 'detach_gpu' || first === 'stop_instance') form.value.gpu_policy.idle.action = first;
}

function policyLabel(kind: PlanPolicyKind, p: PolicyVersion): string {
  let label = `${p.code} v${p.version}`;
  if (kind === 'gpu-pricing') {
    const c = p.config;
    const mode = c.access_mode === 'shared' ? '共享' : '独占';
    label += ` · ${String(c.gpu_model ?? '')} ${mode} ${formatPt(String(c.gpu_hourly_pt ?? ''))} pt/h`;
  }
  if (p.status !== 'active') label += ` · ${enumLabel('catalogStatus', p.status).text}`;
  else if (p.effective_at && Date.parse(p.effective_at) > Date.now()) label += ` · ${formatTime(p.effective_at)} 生效`;
  return label;
}
function usablePolicy(kind: PlanPolicyKind, p: PolicyVersion): boolean {
  if (p.status !== 'active' || (p.effective_at && Date.parse(p.effective_at) > Date.now())) return false;
  return kind !== 'gpu-pricing' || (p.config.gpu_model === form.value.gpu_model.trim() && p.config.access_mode === form.value.access_mode);
}


function policyOptions(kind: PlanPolicyKind) {
  const selected = form.value.policy[kind];
  return (props.refs.policies[kind] ?? [])
    .filter((p) => usablePolicy(kind, p) || p.id === selected)
    .map((p) => ({ disabled: !usablePolicy(kind, p), label: policyLabel(kind, p), value: p.id }));
}

const gpuModelOptions = computed(() => {
  const models = new Set<string>();
  for (const p of props.refs.policies['gpu-pricing'] ?? []) {
    if (p.status === 'active' && typeof p.config.gpu_model === 'string') models.add(p.config.gpu_model);
  }
  return [...models].map((value) => ({ value }));
});

const templateOptions = computed(() =>
  props.refs.templates.value
    .filter((t) => t.status === 'active' || t.id === form.value.init_template_version_id)
    .map((t) => ({ label: props.refs.templateName(t.id), value: t.id })),
);

function baseOf(key: OverrideKey): bigint | null {
  const f = form.value;
  const n = { disk_bytes: f.disk_gib, gpu_count: f.gpu_count, memory_bytes: f.memory_gib, vcpus: f.vcpus }[key];
  if (n === undefined) return null;
  try { return BigInt(toBound(key, n)); } catch { return null; }
}

const problems = computed(() => {
  const f = form.value;
  const out: string[] = [];
  if (!CODE_PATTERN.test(f.code) || !CODE_PATTERN.test(f.version)) {
    out.push('代码与版本须为字母或数字开头，1–64 位字母、数字、_ . -');
  }
  if (!f.name.trim() || [...f.name].length > 128) out.push('名称为 1–128 个字符');
  if (!f.vcpus || !Number.isInteger(f.vcpus) || f.vcpus < 1) out.push('vCPU 须为正整数');
  if (!f.memory_gib || !/^\d+(\.\d{1,3})?$/.test(String(f.memory_gib))) out.push('内存须大于 0，最多 3 位小数');
  if (!f.disk_gib || !/^\d+(\.\d{1,3})?$/.test(String(f.disk_gib))) out.push('磁盘须大于 0，最多 3 位小数');
  if (!Number.isInteger(f.gpu_count) || f.gpu_count < 0) out.push('GPU 数量须为非负整数');
  const price = ptToMicros(f.max_hourly_pt);
  if (!PT_PATTERN.test(f.max_hourly_pt) || price === null || price > 99_999_999_999_999_999_999n) out.push('GPU 单价上限须为 0–99999999999999.999999 pt，最多 6 位小数');
  const idle = f.gpu_policy.idle;
  const c = idleConfig.value;
  if (!Number.isInteger(idle.timeout_seconds) || idle.timeout_seconds < 1 || idle.timeout_seconds > 31_536_000) out.push('空闲时长须为 1–31536000 秒的整数');
  if (c) {
    if (idle.timeout_seconds < Number(c.min_timeout_seconds) || idle.timeout_seconds > Number(c.max_timeout_seconds)) out.push(`空闲时长须在所选政策的 ${String(c.min_timeout_seconds)}–${String(c.max_timeout_seconds)} 秒范围内`);
    if (!idleActions.value.includes(idle.action)) out.push('请选择所选 GPU 空闲政策允许的空闲动作');
    if (idle.enabled && c.enabled !== true) out.push('所选 GPU 空闲政策未启用回收，不能开启空闲回收');
  }
  const model = f.gpu_model.trim();
  const gpuOverride = f.overrides.gpu_count;
  if (isVM.value) {
    if (f.gpu_count !== 0 || model || gpuOverride.enabled) out.push('虚拟机套餐不能提供 GPU');
    if (idle.enabled || f.gpu_policy.rebalance.enabled || f.gpu_policy.rebalance.allow_live) out.push('虚拟机不能开启 GPU 空闲回收、自动均衡或运行中换卡');
    if (f.allowed_user_actions.some((a) => a.startsWith('gpu_')) || GPU_USER_PERMISSIONS.some((k) => f.gpu_user_permissions[k])) out.push('虚拟机不能允许 GPU 操作或 GPU 策略修改权限');
  } else if (model) {
    if ([...model].length > 255) out.push('GPU 型号最长 255 个字符');
    if (f.gpu_count === 0 && !(gpuOverride.enabled && (gpuOverride.max ?? 0) >= 1)) {
      out.push('填写 GPU 型号后，需默认带卡（数量 > 0），或开启“GPU 数量”可选配且上限 ≥ 1');
    }
  } else if (f.gpu_count > 0 || gpuOverride.enabled) {
    out.push('提供 GPU 时必须填写 GPU 型号');
  }
  for (const key of OVERRIDE_KEYS) {
    const o = f.overrides[key];
    if (!o.enabled) continue;
    const label = OVERRIDE_META[key].label;
    if (o.min === undefined || o.max === undefined || o.step === undefined) {
      out.push(`${label}可选配须填写最小值、最大值、步长`);
      continue;
    }
    if (![o.min, o.max, o.step].every((n) => OVERRIDE_META[key].gib ? /^\d+(\.\d{1,3})?$/.test(String(n)) : Number.isInteger(n) && n >= 0)) {
      out.push(`${label}可选配须为非负数${OVERRIDE_META[key].gib ? '，最多 3 位小数' : '整数'}`);
      continue;
    }
    const lo = BigInt(toBound(key, o.min));
    const hi = BigInt(toBound(key, o.max));
    const step = BigInt(toBound(key, o.step));
    if (key !== 'gpu_count' && lo <= 0n) out.push(`${label}可选配最小值须为正`);
    if (step <= 0n) out.push(`${label}可选配步长须大于 0`);
    if (hi < lo) out.push(`${label}可选配最大值不能小于最小值`);
    const base = baseOf(key);
    if (base === null || step <= 0n || (key === 'gpu_count' && base === 0n)) continue;
    if (base < lo || base > hi || (base - lo) % step !== 0n) out.push(`${label}基准值须在可选范围内并与步长对齐`);
  }
  if (f.image_ids.length === 0) out.push('至少选择一个镜像');
  for (const id of f.image_ids) {
    const img = props.refs.images.value.find((i) => i.id === id);
    if (!img) out.push(`镜像 #${id} 不存在，请重新选择`);
    if (img && img.instance_type !== f.instance_type) out.push(`镜像 ${img.name} 类型与套餐不一致`);
    if (img && !compatibleImage(img)) out.push(`镜像 ${img.name} 必须在每个所选网络的节点/项目就绪${hasModel.value ? '，并支持 GPU（含默认不带卡的可选 GPU 套餐）' : ''}`);
  }
  if (f.network_option_ids.length === 0) out.push('至少选择一个网络');
  if (f.node_ids.length === 0) out.push('至少选择一个可用节点');
  for (const id of f.node_ids) {
    if (!props.refs.nodes.value.some((n) => n.id === id)) out.push(`节点 #${id} 不存在，请重新选择`);
    if (!props.refs.bindings.value.some((b) => b.node_id === id && f.network_option_ids.includes(b.id) && b.status === 'active' && (!isVM.value || b.mode !== 'macvlan'))) out.push(`节点 ${props.refs.nodeName(id)} 至少需要选择一个兼容的启用网络`);
  }
  for (const id of f.network_option_ids) {
    if (!networkOptions.value.some((o) => o.value === id && !o.disabled)) out.push(`网络 ${props.refs.bindingName(id)} 未启用、不属于所选节点或不兼容当前实例类型，请重新选择`);
  }
  for (const kind of [...requiredPolicies.value, ...(model ? ['gpu-pricing' as const] : [])]) {
    const p = props.refs.policies[kind]?.find((p) => p.id === f.policy[kind]);
    if (!p || !usablePolicy(kind, p)) out.push(`请选择已生效且兼容的${POLICY_KIND_NAMES[kind]}配置档`);
  }
  return out;
});

function buildBody(): null | PlanInput {
  const f = form.value;
  const { disk_gib: diskGib, memory_gib: memoryGib, vcpus } = f;
  if (diskGib === undefined || memoryGib === undefined || vcpus === undefined) return null;
  const allowed: AllowedOverrides = {};
  for (const key of OVERRIDE_KEYS) {
    const o = f.overrides[key];
    if (o.enabled && o.min !== undefined && o.max !== undefined && o.step !== undefined) {
      allowed[key] = { max: toBound(key, o.max), min: toBound(key, o.min), step: toBound(key, o.step) };
    }
  }
  const model = f.gpu_model.trim();
  const policies: PolicyVersions = {};
  for (const kind of requiredPolicies.value) policies[kind] = f.policy[kind];
  if (model) policies['gpu-pricing'] = f.policy['gpu-pricing'];
  return {
    allowed_overrides: allowed,
    allowed_user_actions: [...f.allowed_user_actions],
    code: f.code,
    gpu: { access_mode: f.access_mode, count: f.gpu_count, max_hourly_pt: f.max_hourly_pt, model: model || null },
    gpu_policy: {
      idle: { ...f.gpu_policy.idle },
      rebalance: { ...f.gpu_policy.rebalance, max_hourly_pt: f.max_hourly_pt },
    },
    gpu_user_permissions: { ...f.gpu_user_permissions },
    image_ids: f.image_ids,
    init_template_version_id: f.init_template_version_id ?? null,
    instance_type: f.instance_type,
    name: f.name,
    network_option_ids: f.network_option_ids,
    node_ids: [...f.node_ids],
    policy_versions: policies,
    resources: {
      disk_bytes: gibToBytes(diskGib),
      memory_bytes: gibToBytes(memoryGib),
      vcpus,
    },
    version: f.version,
  };
}

async function save() {
  tried.value = true;
  if (problems.value.length > 0) return;
  const body = buildBody();
  if (!body) return;
  let r;
  if (props.mode === 'create') {
    r = await action.run(
      (key) => catalogApi.createPlan(body, { key }),
      { success: '已创建套餐（停用）' },
    );
  } else {
    const sourceId = props.source?.id;
    if (!sourceId) return;
    r = await action.run(
      (key) => catalogApi.updatePlan(sourceId, body, { ifMatch: detail.etag.value, key }),
      { onConflict: fill, success: '已保存套餐' },
    );
  }
  if (!r) return;
  open.value = false;
  emit('saved');
}
</script>

<template>
  <Drawer v-model:open="open" destroy-on-close :title="title" width="min(760px, 100vw)">
    <template v-if="!readonly" #extra>
      <Space>
        <Button @click="open = false">取消</Button>
        <Button :loading="action.pending.value" type="primary" @click="save">
          {{ mode === 'create' ? '创建套餐' : '保存套餐' }}
        </Button>
      </Space>
    </template>
    <Alert
      v-if="mode !== 'view'"
      class="mb-3"
      message="新建套餐默认停用，可与其他套餐同时启用。所有字段均可随时编辑；修改仅影响后续创建，已有实例保留创建时配置和范围。停用套餐保存时校验配置格式与引用；启用和保存已启用套餐还需节点可用、引用已发布、网络已验证。保存不会改变启停状态。"
      show-icon
      type="info"
    />
    <Form :disabled="readonly" layout="vertical">
      <Divider class="!mt-0" orientation="left" plain>基本信息</Divider>
      <div class="grid grid-cols-3 gap-x-3">
        <FormItem label="代码" required>
          <Input v-model:value="form.code" class="font-mono" />
        </FormItem>
        <FormItem label="版本" required>
          <Input v-model:value="form.version" class="font-mono" />
        </FormItem>
        <FormItem label="名称" required><Input v-model:value="form.name" :maxlength="128" /></FormItem>
        <FormItem label="实例类型" required>
          <RadioGroup v-model:value="form.instance_type" button-style="solid" @change="onInstanceTypeChange">
            <RadioButton v-for="o in typeOptions" :key="o.value" :value="o.value">{{ o.label }}</RadioButton>
          </RadioGroup>
        </FormItem>
      </div>

      <Divider orientation="left" plain>基准资源</Divider>
      <div class="grid grid-cols-3 gap-x-3">
        <FormItem label="vCPU" required>
          <InputNumber v-model:value="form.vcpus" class="w-full" :min="1" :precision="0" addon-after="核" />
        </FormItem>
        <FormItem label="内存" required>
          <InputNumber v-model:value="form.memory_gib" class="w-full" :min="0" :precision="3" addon-after="GiB" />
        </FormItem>
        <FormItem label="磁盘" required>
          <InputNumber v-model:value="form.disk_gib" class="w-full" :min="0" :precision="3" addon-after="GiB" />
        </FormItem>
      </div>

      <Divider orientation="left" plain>GPU</Divider>
      <Alert
        v-if="isVM"
        class="mb-3"
        message="虚拟机套餐不提供 GPU：数量固定为 0，不能开启 GPU 策略、GPU 操作、GPU 可选配或 GPU 策略修改权限。"
        type="info"
      />
      <div class="grid grid-cols-2 gap-x-3">
        <FormItem
          label="GPU 型号"
          extra="留空 = 不提供 GPU。可从 GPU 定价配置档中选择，或直接输入"
        >
          <AutoComplete v-model:value="form.gpu_model" allow-clear :disabled="readonly || isVM" :options="gpuModelOptions" @change="onGpuModelChange" />
        </FormItem>
        <FormItem label="默认 GPU 数量" extra="0 = 默认不带卡（需开启 GPU 数量可选配让用户自选）">
          <InputNumber
            v-model:value="form.gpu_count"
            class="w-full"
            :disabled="readonly || isVM || !hasModel"
            :min="0"
            :precision="0"
            addon-after="张"
            @change="pruneImages"
          />
        </FormItem>
        <FormItem label="使用方式">
          <RadioGroup v-model:value="form.access_mode" :disabled="readonly || isVM">
            <RadioButton value="exclusive">独占</RadioButton>
            <RadioButton value="shared">共享</RadioButton>
          </RadioGroup>
        </FormItem>
      </div>

      <Divider orientation="left" plain>GPU 策略</Divider>
      <div class="mb-2 font-medium">空闲回收</div>
      <Alert class="mb-3" :message="idleHint" show-icon type="info" />
      <div class="grid grid-cols-1 gap-x-3 sm:grid-cols-3">
        <FormItem label="开启空闲回收">
          <Switch v-model:checked="form.gpu_policy.idle.enabled" aria-label="开启空闲回收" :disabled="readonly || isVM || (!!idleConfig && idleConfig.enabled !== true && !form.gpu_policy.idle.enabled)" />
        </FormItem>
        <FormItem label="空闲时长（秒）" required>
          <InputNumber v-model:value="form.gpu_policy.idle.timeout_seconds" class="w-full" :min="Number(idleConfig?.min_timeout_seconds ?? 1)" :max="Number(idleConfig?.max_timeout_seconds ?? 31536000)" :precision="0" />
        </FormItem>
        <FormItem label="空闲动作" required>
          <Select v-model:value="form.gpu_policy.idle.action" :options="idleActionOptions" />
        </FormItem>
      </div>
      <Button v-if="!readonly && idleConfig" class="mb-3" size="small" @click="applyIdleDefaults">使用所选政策的默认时长与动作</Button>
      <div class="mb-2 font-medium">自动均衡</div>
      <div class="grid grid-cols-1 gap-x-3 sm:grid-cols-2">
        <FormItem label="开启自动均衡" extra="自动换卡仍受平台调度、安全规则及实例状态限制。">
          <Switch v-model:checked="form.gpu_policy.rebalance.enabled" aria-label="开启自动均衡" :disabled="readonly || isVM" />
        </FormItem>
        <FormItem label="允许运行中换卡" extra="仅授权尝试安全换卡，不绕过 GPU 占用与平台安全检查。">
          <Switch v-model:checked="form.gpu_policy.rebalance.allow_live" aria-label="允许运行中换卡" :disabled="readonly || isVM" />
        </FormItem>
        <FormItem label="GPU 分配 / 均衡单价上限" required extra="每张卡每小时 pt，最多 6 位小数。分配与自动均衡共用此上限；0 表示只允许单价为 0 的卡，并非不限价。">
          <Input v-model:value="form.max_hourly_pt" class="font-mono" :disabled="readonly || isVM" addon-after="pt/h" />
        </FormItem>
      </div>

      <Divider orientation="left" plain>用户操作与策略权限</Divider>
      <FormItem label="允许用户执行的操作" extra="新套餐默认仅允许启动、停止、重启和终端。可全部取消；所选权限仍受实例状态和平台能力限制，不影响管理员权限。">
        <CheckboxGroup v-model:value="form.allowed_user_actions" :options="userActionOptions" />
      </FormItem>
      <FormItem label="允许用户修改的 GPU 策略项" extra="默认全部关闭。用户修改策略还需拥有“GPU 策略”操作权限；开启下列权限也不能超出平台政策的范围。虚拟机不能开放这些权限。">
        <div class="grid grid-cols-1 gap-x-6 gap-y-3 sm:grid-cols-2">
          <label v-for="key in GPU_USER_PERMISSIONS" :key="key" class="flex items-center gap-2 text-sm">
            <Switch v-model:checked="form.gpu_user_permissions[key]" :aria-label="GPU_PERMISSION_NAMES[key]" :disabled="readonly || isVM" size="small" />
            {{ GPU_PERMISSION_NAMES[key] ?? key }}
          </label>
        </div>
      </FormItem>

      <Divider orientation="left" plain>可选配（允许用户调整）</Divider>
      <div class="text-muted-foreground mb-2 text-xs">
        未开启的项不写入 allowed_overrides，用户不能调整。用户总能选择不带 GPU，不需要把 0 写进 GPU 数量范围。
      </div>
      <div v-for="key in OVERRIDE_KEYS" :key="key" class="mb-2 flex flex-wrap items-center gap-2">
        <Switch
          v-model:checked="form.overrides[key].enabled"
          :disabled="readonly || (key === 'gpu_count' && (isVM || !hasModel))"
          size="small"
        />
        <span class="w-20">{{ OVERRIDE_META[key].label }}</span>
        <template v-if="form.overrides[key].enabled">
          <InputNumber
            v-model:value="form.overrides[key].min"
            :min="0"
            :precision="OVERRIDE_META[key].gib ? 3 : 0"
            addon-before="最小"
            class="w-40"
          />
          <InputNumber
            v-model:value="form.overrides[key].max"
            :min="0"
            :precision="OVERRIDE_META[key].gib ? 3 : 0"
            addon-before="最大"
            class="w-40"
          />
          <InputNumber
            v-model:value="form.overrides[key].step"
            :min="0"
            :precision="OVERRIDE_META[key].gib ? 3 : 0"
            addon-before="步长"
            class="w-40"
          />
          <span class="text-muted-foreground text-xs">{{ OVERRIDE_META[key].unit }}</span>
        </template>
      </div>

      <Divider orientation="left" plain>节点、网络与镜像</Divider>
      <FormItem label="可用节点" required extra="显式选择此套餐允许落地的节点；每个节点至少配置一个兼容网络。实例实际落在用户所选网络所属的节点，不自动跨节点调度。">
        <Select v-model:value="form.node_ids" mode="multiple" option-filter-prop="label" :options="nodeOptions" placeholder="先选择可用节点" show-search @change="onNodesChange" />
      </FormItem>
      <FormItem label="网络" required extra="仅显示所选节点上启用的网络绑定；启用或保存已启用套餐时须已验证。每个所选节点至少选择一个网络，虚拟机不能使用 macvlan。">
        <Select
          v-model:value="form.network_option_ids"
          :disabled="readonly || form.node_ids.length === 0"
          mode="multiple"
          not-found-content="所选节点没有兼容的启用网络"
          option-filter-prop="label"
          :options="networkOptions"
          placeholder="再选择网络"
          @change="pruneImages"
        />
      </FormItem>
      <FormItem
        label="镜像"
        required
        extra="镜像必须在每个所选网络的节点/项目都就绪；提供 GPU（含可选 GPU）时须全部 GPU 就绪。不同节点可使用不同架构。改变节点、网络或类型时会移除不兼容选择。"
      >
        <Select v-model:value="form.image_ids" :disabled="readonly || form.network_option_ids.length === 0" mode="multiple" option-filter-prop="label" :options="imageOptions" placeholder="最后选择兼容镜像" />
      </FormItem>

      <Divider orientation="left" plain>政策版本</Divider>
      <Alert class="mb-3" type="info" show-icon message="政策随实例创建冻结；额度政策仅控制此实例的宽限与停机规则，不改变用户共享钱包的独立发放政策，也不按实例发放额度。" />
      <div class="grid grid-cols-2 gap-x-3">
        <FormItem v-for="kind in requiredPolicies" :key="kind" :label="POLICY_KIND_NAMES[kind]" required>
          <Select v-model:value="form.policy[kind]" :options="policyOptions(kind)" placeholder="选择配置档" />
        </FormItem>
        <FormItem v-if="hasModel" label="GPU 定价" required>
          <Select v-model:value="form.policy['gpu-pricing']" :options="policyOptions('gpu-pricing')" placeholder="选择已发布版本" />
        </FormItem>
      </div>

      <Divider orientation="left" plain>初始化模板</Divider>
      <FormItem extra="可选。实例首次启动后在实例内执行；用户创建时发送的初始化参数须符合模板参数 schema">
        <Select
          v-model:value="form.init_template_version_id"
          allow-clear
          :options="templateOptions"
          placeholder="不使用初始化模板"
        />
      </FormItem>
    </Form>
    <Alert v-if="tried && problems.length > 0" type="error" show-icon message="请修正以下问题">
      <template #description>
        <ul class="m-0 list-disc pl-4">
          <li v-for="p in problems" :key="p">{{ p }}</li>
        </ul>
      </template>
    </Alert>
  </Drawer>
</template>
