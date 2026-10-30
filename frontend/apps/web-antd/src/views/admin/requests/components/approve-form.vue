<script lang="ts" setup>
import type { CatalogRefs } from '../../catalog/refs';

import type {
  AccessMode,
  CustomAction,
  CustomConfig,
  CustomRequest,
  InstanceType,
  NetworkOption,
  NodeGPU,
} from '#/api/types';

import { computed, ref, watch } from 'vue';

import {
  Alert,
  AutoComplete,
  Button,
  CheckboxGroup,
  Divider,
  Form,
  FormItem,
  Input,
  InputNumber,
  RadioButton,
  RadioGroup,
  Select,
  Space,
  Switch,
  Textarea,
} from 'ant-design-vue';

import { catalogApi, listAll, nodesApi, requestsApi } from '#/api/backend';
import { CUSTOM_ACTIONS, GPU_USER_PERMISSIONS } from '#/api/types';
import ConfirmAction from '#/components/confirm-action.vue';
import { errorToast, trackAccepted, useAction } from '#/composables/api';
import { CUSTOM_ACTION_NAMES, ENUMS, GPU_PERMISSION_NAMES } from '#/locales/enums';
import { bytesToGib, formatPt, gibToBytes, PT_PATTERN } from '#/utils/format';

import { jsonForbidden } from '../../catalog/components/rules';
import { diffKeys, DIFF_CLASS } from './config';

type Permission = (typeof GPU_USER_PERMISSIONS)[number];

interface ApproveForm {
  access_mode: AccessMode;
  actions: CustomAction[];
  disk_bytes: string;
  fixed: boolean;
  fixed_uuids: string[];
  gpu_count: number;
  gpu_model: string;
  image_id: string | undefined;
  init_json: string;
  init_template_version_id: string | undefined;
  instance_type: InstanceType;
  max_hourly_pt: string;
  memory_bytes: string;
  network_option_id: string | undefined;
  node_id: string | undefined;
  permissions: Record<Permission, boolean>;
  vcpus: number | undefined;
  zone_id: string | undefined;
}

const props = defineProps<{ etag: null | string; refs: CatalogRefs; request: CustomRequest }>();
const emit = defineEmits<{ conflict: []; done: [] }>();
/** 与申请不同的字段名，供左侧申请配置同步高亮。 */
const diff = defineModel<Set<string>>('diff', { default: () => new Set() });

function initialForm(c: CustomConfig): ApproveForm {
  return {
    access_mode: c.gpu.access_mode,
    actions: [],
    disk_bytes: c.resources.disk_bytes,
    fixed: false,
    fixed_uuids: [],
    gpu_count: c.gpu.count,
    gpu_model: c.gpu.model ?? '',
    image_id: c.image_id,
    init_json: JSON.stringify(c.init_parameters ?? {}, null, 2),
    init_template_version_id: c.init_template_version_id || undefined,
    instance_type: c.instance_type,
    max_hourly_pt: formatPt(c.gpu.max_hourly_pt),
    memory_bytes: c.resources.memory_bytes,
    network_option_id: c.network_option_id,
    node_id: undefined,
    permissions: {
      allow_live: false,
      idle: false,
      idle_action: false,
      idle_timeout: false,
      max_hourly_pt: false,
      rebalance: false,
    },
    vcpus: c.resources.vcpus,
    zone_id: c.zone_id,
  };
}

const form = ref<ApproveForm>(initialForm(props.request.requested_config));
const note = ref('');
const approveAction = useAction();
const rejectAction = useAction();
watch(
  [form, note],
  () => {
    approveAction.resetKey();
    rejectAction.resetKey();
  },
  { deep: true },
);

const isVM = computed(() => form.value.instance_type === 'virtual-machine');

/** GiB 输入框与字节字符串互转；未改动时保留申请中的原始字节数。 */
function gibModel(key: 'disk_bytes' | 'memory_bytes') {
  return computed<number | undefined>({
    get: () => (/^\d+$/.test(form.value[key]) ? bytesToGib(form.value[key]) : undefined),
    set: (v) => {
      form.value[key] = v === undefined || v === null ? '' : gibToBytes(v);
    },
  });
}
const memoryGib = gibModel('memory_bytes');
const diskGib = gibModel('disk_bytes');

watch(isVM, (vm) => {
  if (vm) {
    form.value.gpu_count = 0;
    form.value.gpu_model = '';
  }
});
watch(
  () => form.value.gpu_count,
  (n) => {
    if (n === 0) {
      form.value.gpu_model = '';
      form.value.fixed = false;
    }
    form.value.fixed_uuids = form.value.fixed_uuids.slice(0, n);
  },
);

/* ---------- 下拉选项 ---------- */

const zoneOptions = computed(() =>
  props.refs.zones.value
    .filter((z) => z.status === 'active' || z.id === form.value.zone_id)
    .map((z) => ({ label: props.refs.zoneName(z.id), value: z.id })),
);

const imageOptions = computed(() =>
  props.refs.images.value
    .filter((i) => i.instance_type === form.value.instance_type)
    .filter((i) => i.status === 'active' || i.id === form.value.image_id)
    .map((i) => ({
      disabled: i.status !== 'active' || (form.value.gpu_count > 0 && !i.gpu_ready),
      label: `${props.refs.imageName(i.id)} · ${i.architecture}${i.gpu_ready ? ' · GPU 就绪' : ''}`,
      value: i.id,
    })),
);

const networkOptions = ref<NetworkOption[]>([]);
let networkSeq = 0;
watch(
  () => [form.value.zone_id, form.value.instance_type] as const,
  async ([zone, type]) => {
    const mine = ++networkSeq;
    networkOptions.value = [];
    if (!zone) return;
    try {
      const items = await listAll((q) => catalogApi.networkOptions({ ...q, instance_type: type, zone_id: zone }));
      if (mine === networkSeq) networkOptions.value = items;
    } catch (error) {
      errorToast(error);
    }
  },
  { immediate: true },
);
const networkSelectOptions = computed(() =>
  networkOptions.value.map((o) => ({ label: props.refs.bindingName(o.id), value: o.id })),
);
const networkInvalid = computed(
  () => !!form.value.network_option_id && !networkOptions.value.some((o) => o.id === form.value.network_option_id),
);

const nodeOptions = computed(() =>
  props.refs.nodes.value
    .filter((n) => n.zone_id === form.value.zone_id)
    .map((n) => ({
      label: `${n.name}（#${n.id}）${n.administrative_state === 'active' ? '' : ` · ${ENUMS.adminState?.[n.administrative_state]?.text ?? n.administrative_state}`}`,
      value: n.id,
    })),
);
watch(
  () => form.value.zone_id,
  () => {
    if (form.value.node_id && !nodeOptions.value.some((o) => o.value === form.value.node_id)) {
      form.value.node_id = undefined;
    }
  },
);

const nodeGpus = ref<NodeGPU[]>([]);
watch(
  () => form.value.node_id,
  async (id) => {
    nodeGpus.value = [];
    form.value.fixed_uuids = [];
    if (!id) return;
    try {
      nodeGpus.value = await listAll((q) => nodesApi.gpus(id, q));
    } catch (error) {
      errorToast(error);
    }
  },
);
const gpuModelOptions = computed(() => [...new Set(nodeGpus.value.map((g) => g.model))].map((value) => ({ value })));
const uuidOptions = computed(() =>
  nodeGpus.value
    .filter((g) => g.status === 'available' && (!form.value.gpu_model || g.model === form.value.gpu_model))
    .map((g) => ({ label: `${g.uuid} · ${g.model} · ${g.pci_address}`, value: g.uuid })),
);

const templateOptions = computed(() =>
  props.refs.templates.value
    .filter((t) => t.status === 'active')
    .map((t) => ({ label: props.refs.templateName(t.id), value: t.id })),
);
const selectedTemplate = computed(() =>
  props.refs.templates.value.find((t) => t.id === form.value.init_template_version_id),
);

/* ---------- 组装与校验 ---------- */

const params = computed<{ error: string } | { value: Record<string, unknown> }>(() => {
  try {
    const v: unknown = JSON.parse(form.value.init_json || '{}');
    if (typeof v !== 'object' || v === null || Array.isArray(v)) return { error: '初始化参数必须是 JSON 对象' };
    return { value: v as Record<string, unknown> };
  } catch {
    return { error: '初始化参数不是合法 JSON' };
  }
});

const config = computed<CustomConfig>(() => {
  const f = form.value;
  const count = f.gpu_count;
  const c: CustomConfig = {
    allowed_user_actions: CUSTOM_ACTIONS.filter((a) => f.actions.includes(a)),
    gpu: {
      access_mode: f.access_mode,
      count,
      max_hourly_pt: f.max_hourly_pt,
      model: count > 0 ? f.gpu_model.trim() || null : null,
    },
    gpu_user_permissions: { ...f.permissions },
    image_id: f.image_id ?? '',
    init_parameters: 'value' in params.value ? params.value.value : {},
    instance_type: f.instance_type,
    network_option_id: f.network_option_id ?? '',
    resources: { disk_bytes: f.disk_bytes, memory_bytes: f.memory_bytes, vcpus: f.vcpus ?? 0 },
    zone_id: f.zone_id ?? '',
  };
  if (count > 0 && f.fixed) c.gpu.fixed_uuids = [...f.fixed_uuids];
  if (f.node_id) c.node_id = f.node_id;
  if (f.init_template_version_id) c.init_template_version_id = f.init_template_version_id;
  return c;
});

watch(config, (c) => (diff.value = diffKeys(props.request.requested_config, c)), { immediate: true });
const cls = (key: string) => (diff.value.has(key) ? DIFF_CLASS : '');

const problems = computed(() => {
  const f = form.value;
  const c = config.value;
  const out: string[] = [];
  if (!f.zone_id) out.push('请选择可用区');
  if (!f.image_id) out.push('请选择镜像');
  if (!f.network_option_id) out.push('请选择网络');
  else if (networkInvalid.value) out.push('所选网络不是该可用区、该类型可用的已验证网络');
  if (!f.vcpus || !Number.isInteger(f.vcpus) || f.vcpus < 1) out.push('vCPU 须为正整数');
  if (!/^[1-9]\d*$/.test(f.memory_bytes)) out.push('内存须大于 0');
  if (!/^[1-9]\d*$/.test(f.disk_bytes)) out.push('磁盘须大于 0');
  if (!PT_PATTERN.test(f.max_hourly_pt)) out.push('GPU 单价上限须为非负 pt，最多 6 位小数');
  if (f.gpu_count > 0) {
    if (isVM.value) out.push('虚拟机不能带 GPU');
    if (!c.gpu.model || [...c.gpu.model].length > 255) out.push('带 GPU 时须填写 GPU 型号（≤255 字符）');
    const img = props.refs.images.value.find((i) => i.id === f.image_id);
    if (img && !img.gpu_ready) out.push('带 GPU 时须选择 GPU 就绪镜像');
    if (f.fixed) {
      if (f.fixed_uuids.length !== f.gpu_count) out.push(`指定 GPU 的数量须等于 GPU 数量（${f.gpu_count}）`);
      if (new Set(f.fixed_uuids).size !== f.fixed_uuids.length) out.push('指定 GPU 不能重复');
      if (f.fixed_uuids.some((u) => !u.trim() || u.trim() !== u || u.length > 128)) out.push('GPU UUID 格式不正确');
    }
  }
  const p = params.value;
  if ('error' in p) out.push(p.error);
  else {
    if (!f.init_template_version_id && Object.keys(p.value).length > 0) out.push('未选择初始化模板时，初始化参数必须为空对象 {}');
    const bad = jsonForbidden(p.value);
    if (bad.length > 0) out.push(`初始化参数不能包含：${bad.join('、')}`);
  }
  return out;
});

/* ---------- 提交 ---------- */

const tried = ref(false);
const noteValid = computed(() => note.value.trim().length > 0 && note.value.length <= 4000);
const approveOpen = ref(false);
const rejectOpen = ref(false);

function askApprove() {
  tried.value = true;
  if (problems.value.length === 0 && noteValid.value) approveOpen.value = true;
}
function askReject() {
  tried.value = true;
  if (noteValid.value) rejectOpen.value = true;
}

async function approve() {
  const r = await approveAction.run(
    (key) =>
      requestsApi.approve(
        props.request.id,
        { approved_config: config.value, review_note: note.value.trim() },
        { ifMatch: props.etag, key },
      ),
    { onConflict: () => emit('conflict'), success: '已批准，交付任务已加入任务中心' },
  );
  if (!r) return false;
  trackAccepted(r.data, `交付自定义实例 ${props.request.name}`);
  emit('done');
  return true;
}

async function reject() {
  const r = await rejectAction.run(
    (key) => requestsApi.reject(props.request.id, note.value.trim(), { ifMatch: props.etag, key }),
    { onConflict: () => emit('conflict'), success: '已拒绝' },
  );
  if (!r) return false;
  emit('done');
  return true;
}

const actionOptions = CUSTOM_ACTIONS.map((a) => ({ label: CUSTOM_ACTION_NAMES[a] ?? a, value: a }));
</script>

<template>
  <div>
    <Alert
      class="mb-3"
      show-icon
      type="warning"
      message="自定义实例占用该成员的“实例数上限”（max_instances）与“常驻实例上限”（max_resident_instances），配额不足时批准会失败（quota_exceeded）。计费方式由后端定为免额度（admin_exempt），这里没有计费选项。"
    />
    <Form layout="vertical">
      <div class="grid grid-cols-2 gap-x-3">
        <FormItem label="实例类型" :class="cls('instance_type')">
          <RadioGroup v-model:value="form.instance_type" button-style="solid">
            <RadioButton value="container">容器</RadioButton>
            <RadioButton value="virtual-machine">虚拟机</RadioButton>
          </RadioGroup>
        </FormItem>
        <FormItem label="可用区" required :class="cls('zone_id')">
          <Select v-model:value="form.zone_id" :options="zoneOptions" />
        </FormItem>
      </div>
      <FormItem label="镜像" required :class="cls('image_id')">
        <Select v-model:value="form.image_id" :options="imageOptions" show-search option-filter-prop="label" />
      </FormItem>
      <FormItem
        label="网络"
        required
        :class="cls('network_option_id')"
        :validate-status="networkInvalid ? 'error' : undefined"
        :help="networkInvalid ? '所选网络在该可用区/类型下不可用，请重新选择' : undefined"
      >
        <Select
          v-model:value="form.network_option_id"
          not-found-content="该可用区没有已验证的可用网络"
          :options="networkSelectOptions"
        />
      </FormItem>

      <Divider orientation="left" plain>资源</Divider>
      <div class="grid grid-cols-3 gap-x-3">
        <FormItem label="vCPU" required :class="cls('vcpus')">
          <InputNumber v-model:value="form.vcpus" addon-after="核" class="w-full" :min="1" :precision="0" />
        </FormItem>
        <FormItem label="内存" required :class="cls('memory_bytes')">
          <InputNumber v-model:value="memoryGib" addon-after="GiB" class="w-full" :min="0" :precision="3" />
        </FormItem>
        <FormItem label="磁盘" required :class="cls('disk_bytes')">
          <InputNumber v-model:value="diskGib" addon-after="GiB" class="w-full" :min="0" :precision="3" />
        </FormItem>
      </div>

      <Divider orientation="left" plain>GPU</Divider>
      <div class="grid grid-cols-2 gap-x-3">
        <FormItem label="数量" :class="cls('gpu_count')" :extra="isVM ? '虚拟机不能带 GPU' : '0 = 不带 GPU'">
          <InputNumber v-model:value="form.gpu_count" addon-after="张" class="w-full" :disabled="isVM" :min="0" :precision="0" />
        </FormItem>
        <FormItem label="型号" :class="cls('gpu_model')" :required="form.gpu_count > 0">
          <AutoComplete
            v-model:value="form.gpu_model"
            :disabled="form.gpu_count === 0"
            :options="gpuModelOptions"
            placeholder="如 NVIDIA A100-SXM4-80GB"
          />
        </FormItem>
        <FormItem label="使用方式" :class="cls('access_mode')">
          <RadioGroup v-model:value="form.access_mode">
            <RadioButton value="exclusive">独占</RadioButton>
            <RadioButton value="shared">共享</RadioButton>
          </RadioGroup>
        </FormItem>
        <FormItem label="单价上限" :class="cls('max_hourly_pt')" extra="每张卡每小时 pt；分配时不选更贵的卡">
          <Input v-model:value="form.max_hourly_pt" addon-after="pt/h" class="font-mono" />
        </FormItem>
      </div>

      <Divider orientation="left" plain>放置（可选）</Divider>
      <FormItem label="指定节点" extra="留空由后端选择">
        <Select v-model:value="form.node_id" allow-clear :options="nodeOptions" placeholder="不指定" />
      </FormItem>
      <FormItem v-if="form.gpu_count > 0" label="指定 GPU" extra="默认不指定，由后端选最低负载的卡；指定时数量须等于 GPU 数量">
        <Switch v-model:checked="form.fixed" size="small" />
        <Select
          v-if="form.fixed"
          v-model:value="form.fixed_uuids"
          class="mt-2"
          :max-tag-count="form.gpu_count"
          mode="tags"
          :options="uuidOptions"
          :placeholder="form.node_id ? '选择该节点上可用的 GPU，或输入 UUID' : '输入 GPU UUID（先指定节点可从列表选择）'"
        />
      </FormItem>

      <Divider orientation="left" plain>初始化</Divider>
      <FormItem label="初始化模板" :class="cls('init_template_version_id')">
        <Select
          v-model:value="form.init_template_version_id"
          allow-clear
          :options="templateOptions"
          placeholder="不使用初始化模板"
        />
      </FormItem>
      <FormItem
        label="初始化参数（JSON 对象）"
        :class="cls('init_parameters')"
        :extra="form.init_template_version_id ? '须符合下方模板参数 schema' : '未选模板时必须为 {}'"
      >
        <Textarea v-model:value="form.init_json" :auto-size="{ minRows: 3, maxRows: 12 }" class="font-mono text-xs" spellcheck="false" />
      </FormItem>
      <pre
        v-if="selectedTemplate"
        class="bg-muted -mt-2 mb-4 max-h-48 overflow-auto rounded p-2 font-mono text-xs"
      >{{ JSON.stringify(selectedTemplate.parameter_schema, null, 2) }}</pre>

      <Divider orientation="left" plain>成员权限</Divider>
      <FormItem label="成员可执行的操作" extra="未勾选的操作成员不能执行（包括启动、停止）">
        <CheckboxGroup v-model:value="form.actions" :options="actionOptions" />
      </FormItem>
      <FormItem label="成员可修改的 GPU 策略项">
        <div class="grid grid-cols-3 gap-2">
          <label v-for="p in GPU_USER_PERMISSIONS" :key="p" class="flex items-center gap-2">
            <Switch v-model:checked="form.permissions[p]" size="small" />
            <span>{{ GPU_PERMISSION_NAMES[p] ?? p }}</span>
          </label>
        </div>
      </FormItem>

      <Divider orientation="left" plain>审批意见</Divider>
      <FormItem
        required
        :validate-status="tried && !noteValid ? 'error' : undefined"
        :help="tried && !noteValid ? '请填写审批意见（1–4000 字符）' : '成员可见，并记入审计'"
      >
        <Textarea v-model:value="note" :auto-size="{ minRows: 2, maxRows: 6 }" :maxlength="4000" show-count />
      </FormItem>
    </Form>

    <Alert v-if="tried && problems.length > 0" class="mb-3" message="批准前请修正" show-icon type="error">
      <template #description>
        <ul class="m-0 list-disc pl-4">
          <li v-for="p in problems" :key="p">{{ p }}</li>
        </ul>
      </template>
    </Alert>

    <Space>
      <Button :loading="approveAction.pending.value" type="primary" @click="askApprove">批准并交付</Button>
      <Button danger :loading="rejectAction.pending.value" @click="askReject">拒绝</Button>
      <span v-if="diff.size > 0" class="text-xs text-amber-600">已修改 {{ diff.size }} 项（高亮显示）</span>
    </Space>

    <ConfirmAction
      v-model:open="approveOpen"
      :description="`将按右侧配置为成员创建实例并开始交付${diff.size > 0 ? `（与申请相比修改了 ${diff.size} 项）` : ''}。批准后即使交付失败也不会退回待审批。`"
      :on-confirm="approve"
      title="批准申请"
    />
    <ConfirmAction
      v-model:open="rejectOpen"
      danger
      description="拒绝后申请结束，成员可看到审批意见。"
      :on-confirm="reject"
      title="拒绝申请"
    />
  </div>
</template>
