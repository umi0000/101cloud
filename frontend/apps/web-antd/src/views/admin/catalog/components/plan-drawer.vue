<script lang="ts" setup>
import type { CatalogRefs } from '../refs';

import type {
  AccessMode,
  AllowedOverrides,
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
import { errorToast, useAction, useResource } from '#/composables/api';
import { enumLabel, ENUMS, POLICY_KIND_NAMES } from '#/locales/enums';
import { bytesToGib, formatPt, formatTime, gibToBytes, PT_PATTERN } from '#/utils/format';

import { CODE_PATTERN } from './rules';

type PlanPolicyKind = Exclude<PolicyKind, 'metrics'>;
type Mode = 'create' | 'edit' | 'view';

interface OverrideForm {
  enabled: boolean;
  max: number | undefined;
  min: number | undefined;
  step: number | undefined;
}

interface PlanForm {
  access_mode: AccessMode;
  code: string;
  disk_gib: number | undefined;
  gpu_count: number;
  gpu_model: string;
  image_ids: string[];
  init_template_version_id: string | undefined;
  instance_type: InstanceType;
  max_hourly_pt: string;
  memory_gib: number | undefined;
  name: string;
  network_option_ids: string[];
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
const REQUIRED_POLICIES: PlanPolicyKind[] = ['pricing', 'credit', 'gpu-idle', 'gpu-scheduling'];

const typeOptions = Object.entries(ENUMS.instanceType ?? {}).map(([value, l]) => ({ label: l.text, value }));

const emptyOverride = (): OverrideForm => ({ enabled: false, max: undefined, min: undefined, step: undefined });

function emptyForm(): PlanForm {
  return {
    access_mode: 'exclusive',
    code: '',
    disk_gib: undefined,
    gpu_count: 0,
    gpu_model: '',
    image_ids: [],
    init_template_version_id: undefined,
    instance_type: 'container',
    max_hourly_pt: '0',
    memory_gib: undefined,
    name: '',
    network_option_ids: [],
    overrides: {
      disk_bytes: emptyOverride(),
      gpu_count: emptyOverride(),
      memory_bytes: emptyOverride(),
      vcpus: emptyOverride(),
    },
    policy: { credit: undefined, 'gpu-idle': undefined, 'gpu-pricing': undefined, 'gpu-scheduling': undefined, pricing: undefined },
    vcpus: undefined,
    version: '',
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
    code: p.code,
    disk_gib: bytesToGib(p.resources.disk_bytes),
    gpu_count: p.gpu.count,
    gpu_model: p.gpu.model ?? '',
    image_ids: [...p.image_ids],
    init_template_version_id: p.init_template_version_id ?? undefined,
    instance_type: p.instance_type,
    max_hourly_pt: formatPt(p.gpu.max_hourly_pt),
    memory_gib: bytesToGib(p.resources.memory_bytes),
    name: p.name,
    network_option_ids: [...p.network_option_ids],
    overrides,
    policy: {
      credit: p.policy_versions.credit,
      'gpu-idle': p.policy_versions['gpu-idle'],
      'gpu-pricing': p.policy_versions['gpu-pricing'],
      'gpu-scheduling': p.policy_versions['gpu-scheduling'],
      pricing: p.policy_versions.pricing,
    },
    vcpus: p.resources.vcpus,
    version: copy ? '' : p.version,
  };
}

const form = ref<PlanForm>(emptyForm());
const tried = ref(false);
const action = useAction();
const detail = useResource(() => (props.source ? catalogApi.plan(props.source.id) : null));
const readonly = computed(() => props.mode === 'view');
const title = computed(() => {
  if (props.mode === 'create') return props.source ? `基于 ${props.source.code} 新建版本` : '新建套餐';
  const p = detail.data.value ?? props.source;
  return `${props.mode === 'edit' ? '编辑草稿' : '套餐'} ${p?.code ?? ''} v${p?.version ?? ''}`;
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

watch(
  () => form.value.instance_type,
  (type) => {
    const f = form.value;
    if (type === 'virtual-machine') {
      f.gpu_count = 0;
      f.gpu_model = '';
      f.overrides.gpu_count = emptyOverride();
    }
    f.image_ids = f.image_ids.filter((id) => props.refs.images.value.find((i) => i.id === id)?.instance_type === type);
    f.network_option_ids = f.network_option_ids.filter((id) => networkOptions.value.some((o) => o.value === id));
  },
);
watch(hasModel, (v) => {
  if (!v) {
    form.value.gpu_count = 0;
    form.value.overrides.gpu_count = emptyOverride();
  }
});

const imageOptions = computed(() =>
  props.refs.images.value
    .filter((i) => i.instance_type === form.value.instance_type)
    .filter((i) => i.status === 'active' || form.value.image_ids.includes(i.id))
    .map((i) => ({
      disabled: form.value.gpu_count > 0 && !i.gpu_ready,
      label: `${i.name} · ${i.os_name} ${i.os_release} · ${i.architecture}${i.gpu_ready ? ' · GPU 就绪' : ''}${i.status === 'active' ? '' : ' · 未启用'}`,
      value: i.id,
    })),
);

const networkOptions = computed(() =>
  props.refs
    .activeBindings()
    .filter((b) => !isVM.value || b.mode !== 'macvlan')
    .map((b) => ({
      label: `${props.refs.bindingName(b.id)}${b.validation_status === 'verified' ? '' : `（${enumLabel('validation', b.validation_status).text}）`}`,
      value: b.id,
    })),
);

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

function policyOptions(kind: PlanPolicyKind) {
  const selected = form.value.policy[kind];
  return (props.refs.policies[kind] ?? [])
    .filter((p) => p.status === 'active' || p.id === selected)
    .map((p) => ({ label: policyLabel(kind, p), value: p.id }));
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
  return n === undefined ? null : BigInt(toBound(key, n));
}

const problems = computed(() => {
  const f = form.value;
  const out: string[] = [];
  if (props.mode === 'create' && (!CODE_PATTERN.test(f.code) || !CODE_PATTERN.test(f.version))) {
    out.push('代码与版本须为字母或数字开头，1–64 位字母、数字、_ . -');
  }
  if (!f.name.trim() || [...f.name].length > 128) out.push('名称为 1–128 个字符');
  if (!f.vcpus || !Number.isInteger(f.vcpus) || f.vcpus < 1) out.push('vCPU 须为正整数');
  if (!f.memory_gib || f.memory_gib <= 0) out.push('内存须大于 0');
  if (!f.disk_gib || f.disk_gib <= 0) out.push('磁盘须大于 0');
  if (!PT_PATTERN.test(f.max_hourly_pt)) out.push('GPU 单价上限须为非负 pt，最多 6 位小数');
  const model = f.gpu_model.trim();
  const gpuOverride = f.overrides.gpu_count;
  if (isVM.value) {
    if (f.gpu_count !== 0 || model || gpuOverride.enabled) out.push('虚拟机套餐不能提供 GPU');
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
    if (img && img.instance_type !== f.instance_type) out.push(`镜像 ${img.name} 类型与套餐不一致`);
    if (img && f.gpu_count > 0 && !img.gpu_ready) out.push(`默认带 GPU 的套餐只能选 GPU 就绪镜像（${img.name}）`);
  }
  if (f.network_option_ids.length === 0) out.push('至少选择一个网络');
  for (const kind of REQUIRED_POLICIES) {
    if (!f.policy[kind]) out.push(`请选择${POLICY_KIND_NAMES[kind]}政策`);
  }
  if (model && !f.policy['gpu-pricing']) out.push('填写 GPU 型号时必须选择 GPU 定价政策');
  return out;
});

function buildBody(): null | Omit<PlanInput, 'code' | 'version'> {
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
  for (const kind of REQUIRED_POLICIES) policies[kind] = f.policy[kind];
  if (model) policies['gpu-pricing'] = f.policy['gpu-pricing'];
  return {
    allowed_overrides: allowed,
    gpu: { access_mode: f.access_mode, count: f.gpu_count, max_hourly_pt: f.max_hourly_pt, model: model || null },
    image_ids: f.image_ids,
    init_template_version_id: f.init_template_version_id ?? null,
    instance_type: f.instance_type,
    name: f.name,
    network_option_ids: f.network_option_ids,
    policy_versions: policies,
    resources: {
      disk_bytes: gibToBytes(diskGib),
      memory_bytes: gibToBytes(memoryGib),
      vcpus,
    },
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
      (key) => catalogApi.createPlan({ ...body, code: form.value.code, version: form.value.version }, { key }),
      { success: '已创建草稿' },
    );
  } else {
    const sourceId = props.source?.id;
    if (!sourceId) return;
    r = await action.run(
      (key) => catalogApi.updatePlan(sourceId, body, { ifMatch: detail.etag.value, key }),
      { onConflict: fill, success: '已保存草稿' },
    );
  }
  if (!r) return;
  open.value = false;
  emit('saved');
}
</script>

<template>
  <Drawer v-model:open="open" destroy-on-close :title="title" :width="760">
    <template v-if="!readonly" #extra>
      <Space>
        <Button @click="open = false">取消</Button>
        <Button :loading="action.pending.value" type="primary" @click="save">
          {{ mode === 'create' ? '创建草稿' : '保存草稿' }}
        </Button>
      </Space>
    </template>
    <Alert
      v-if="mode !== 'view'"
      class="mb-3"
      message="草稿保存时检查格式和引用是否存在；发布时后端再校验引用都已发布、网络已验证、镜像兼容。发布后不能修改，只能退役并新建版本。"
      show-icon
      type="info"
    />
    <Form :disabled="readonly" layout="vertical">
      <Divider class="!mt-0" orientation="left" plain>基本信息</Divider>
      <div class="grid grid-cols-3 gap-x-3">
        <FormItem label="代码" required>
          <Input v-model:value="form.code" class="font-mono" :disabled="mode !== 'create' || !!source" />
        </FormItem>
        <FormItem label="版本" required>
          <Input v-model:value="form.version" class="font-mono" :disabled="mode !== 'create'" />
        </FormItem>
        <FormItem label="名称" required><Input v-model:value="form.name" :maxlength="128" /></FormItem>
        <FormItem label="实例类型" required>
          <RadioGroup v-model:value="form.instance_type" button-style="solid">
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
        message="虚拟机套餐不提供 GPU（数量固定 0、型号为空、不能配置 GPU 数量可选配）。"
        type="info"
      />
      <div class="grid grid-cols-2 gap-x-3">
        <FormItem
          label="GPU 型号"
          extra="留空 = 不提供 GPU。可从已发布的 GPU 定价政策中选择，或直接输入"
        >
          <AutoComplete v-model:value="form.gpu_model" allow-clear :disabled="readonly || isVM" :options="gpuModelOptions" />
        </FormItem>
        <FormItem label="默认 GPU 数量" extra="0 = 默认不带卡（需开启 GPU 数量可选配让用户自选）">
          <InputNumber
            v-model:value="form.gpu_count"
            class="w-full"
            :disabled="readonly || isVM || !hasModel"
            :min="0"
            :precision="0"
            addon-after="张"
          />
        </FormItem>
        <FormItem label="使用方式">
          <RadioGroup v-model:value="form.access_mode" :disabled="readonly || isVM">
            <RadioButton value="exclusive">独占</RadioButton>
            <RadioButton value="shared">共享</RadioButton>
          </RadioGroup>
        </FormItem>
        <FormItem label="GPU 单价上限" extra="每张卡每小时 pt；分配时不选单价高于此值的卡">
          <Input v-model:value="form.max_hourly_pt" class="font-mono" :disabled="readonly || isVM" addon-after="pt/h" />
        </FormItem>
      </div>

      <Divider orientation="left" plain>可选配（允许用户调整）</Divider>
      <div class="text-muted-foreground mb-2 text-xs">
        未开启的项不写入 allowed_overrides，用户不能调整。用户总能选择不带 GPU，不需要把 0 写进 GPU 数量范围。
      </div>
      <div v-for="key in OVERRIDE_KEYS" :key="key" class="mb-2 flex items-center gap-2">
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

      <Divider orientation="left" plain>镜像与网络</Divider>
      <FormItem
        label="镜像"
        required
        extra="类型须与套餐一致；默认带 GPU 的套餐只能选 GPU 就绪镜像。GPU 可选的套餐允许混选，用户带卡时后端要求所选镜像 GPU 就绪"
      >
        <Select v-model:value="form.image_ids" mode="multiple" :options="imageOptions" placeholder="选择镜像" />
      </FormItem>
      <FormItem label="网络" required extra="所有节点上启用中的网络绑定；发布时要求已验证。虚拟机不能使用 macvlan">
        <Select
          v-model:value="form.network_option_ids"
          mode="multiple"
          not-found-content="没有启用中的网络绑定"
          :options="networkOptions"
          placeholder="选择网络"
        />
      </FormItem>

      <Divider orientation="left" plain>政策版本</Divider>
      <div class="grid grid-cols-2 gap-x-3">
        <FormItem v-for="kind in REQUIRED_POLICIES" :key="kind" :label="POLICY_KIND_NAMES[kind]" required>
          <Select v-model:value="form.policy[kind]" :options="policyOptions(kind)" placeholder="选择已发布版本" />
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
