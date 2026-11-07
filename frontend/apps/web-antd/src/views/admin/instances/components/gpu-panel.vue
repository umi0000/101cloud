<script lang="ts" setup>
/** GPU 标签页（仅容器）：分配列表 + 挂载 / 移除 / 换卡。管理员可指定 fixed_uuids。 */
import type { AttachBody } from '#/api/backend';
import type {
  GPUAllocation,
  GPUOption,
  Instance,
  LatestGPUMetric,
  NodeGPU,
  PolicyVersion,
} from '#/api/types';

import { computed, onMounted, ref, shallowRef, watch } from 'vue';

import {
  Alert,
  Button,
  Card,
  Checkbox,
  Form,
  FormItem,
  Input,
  InputNumber,
  Select,
  Space,
  Switch,
  Table,
  Tag,
  Tooltip,
} from 'ant-design-vue';

import {
  catalogApi,
  instancesApi,
  listAll,
  nodesApi,
  policiesApi,
} from '#/api/backend';
import ConfirmAction from '#/components/confirm-action.vue';
import StatusTag from '#/components/status-tag.vue';
import TimeText from '#/components/time-text.vue';
import {
  errorToast,
  trackAccepted,
  useAction,
  usePolling,
} from '#/composables/api';
import { enumLabel, instanceTaskState } from '#/locales/enums';
import {
  formatBytes,
  formatMib,
  formatPercent,
  formatPt,
  PT_PATTERN,
  shortId,
} from '#/utils/format';

import { ACCESS_MODE_NAMES, yesNo } from './shared';

const props = defineProps<{ etag: null | string; instance: Instance }>();
const emit = defineEmits<{ changed: [] }>();

const allocations = shallowRef<GPUAllocation[]>([]);
const live = shallowRef<Record<string, LatestGPUMetric>>({});
const loading = ref(false);
const showHistory = ref(false);
const allocationsLoaded = ref(false);
const selected = ref<string[]>([]);

async function loadAllocations() {
  loading.value = true;
  try {
    allocations.value = (
      await instancesApi.gpuAllocations(props.instance.id)
    ).data;
    allocationsLoaded.value = true;
    const ids = new Set(allocations.value.map((a) => a.id));
    selected.value = selected.value.filter((id) => ids.has(id));
  } catch (error) {
    errorToast(error);
  } finally {
    loading.value = false;
  }
}

async function loadLive() {
  try {
    const r = await instancesApi.latest(props.instance.id);
    live.value = Object.fromEntries(r.data.gpus.map((g) => [g.uuid, g]));
  } catch {
    live.value = {};
  }
}

usePolling(
  () => Promise.all([loadAllocations(), loadLive()]),
  () => 10_000,
);
onMounted(() => Promise.all([loadAllocations(), loadLive()]));
watch(
  () => props.instance.revision,
  () => void loadAllocations(),
);

const HISTORY: Record<string, true> = {
  cancelled: true,
  failed: true,
  released: true,
};
const rows = computed(() =>
  allocations.value.filter((a) => showHistory.value || !HISTORY[a.status]),
);
const activeSelected = computed(() =>
  allocations.value.filter(
    (a) => selected.value.includes(a.id) && a.status === 'active',
  ),
);

const busy = computed(() => !!props.instance.active_task_id);
const taskState = computed(() => instanceTaskState(props.instance));
const running = computed(() => props.instance.observed_state === 'running');

const columns = [
  { dataIndex: 'model', title: '型号' },
  { key: 'uuid', title: 'UUID' },
  { key: 'status', title: '状态' },
  { key: 'flags', title: '已配置 / 已观测' },
  { key: 'sm', title: 'SM' },
  { key: 'mem', title: '显存' },
  { key: 'proc', title: '进程数' },
  { key: 'price', title: '单价' },
  { key: 'time', title: '分配 / 释放' },
];

const rowSelection = computed(() => ({
  getCheckboxProps: (r: GPUAllocation) => ({ disabled: r.status !== 'active' }),
  onChange: (keys: (number | string)[]) => (selected.value = keys.map(String)),
  selectedRowKeys: selected.value,
}));

/* ---------- 选项：GPU 型号与节点 GPU ---------- */

const gpuOptions = shallowRef<GPUOption[]>([]);
const nodeGPUs = shallowRef<NodeGPU[]>([]);
const pricingProfiles = shallowRef<PolicyVersion[]>([]);
const boundPricing = shallowRef<null | PolicyVersion>(null);
const pricingLoading = ref(false);
const pricingId = ref<string>();
const pricingAction = useAction();
const missingPricing = computed(
  () => !props.instance.policy_versions['gpu-pricing'],
);
const needsPricingSetup = computed(
  () => props.instance.product_type === 'custom' && missingPricing.value,
);
const pricingOptions = computed(() =>
  pricingProfiles.value
    .filter(
      (p) =>
        p.status === 'active' &&
        (!p.effective_at || Date.parse(p.effective_at) <= Date.now()),
    )
    .map((p) => ({
      label: `${p.code} v${p.version} · ${String(p.config.gpu_model)} · ${ACCESS_MODE_NAMES[p.config.access_mode as 'exclusive' | 'shared']} · ${formatPt(String(p.config.gpu_hourly_pt))} pt/h`,
      value: p.id,
    })),
);
const canSetPricing = computed(
  () =>
    needsPricingSetup.value &&
    allocationsLoaded.value &&
    allocations.value.length === 0 &&
    !busy.value &&
    !pricingLoading.value &&
    pricingOptions.value.some((p) => p.value === pricingId.value),
);
watch(pricingId, () => pricingAction.resetKey());
let pricingSeq = 0;
async function loadPricing() {
  const mine = ++pricingSeq;
  pricingLoading.value = true;
  boundPricing.value = null;
  try {
    const id = props.instance.policy_versions['gpu-pricing'];
    if (id) {
      const r = await policiesApi.get('gpu-pricing', id);
      if (mine === pricingSeq) boundPricing.value = r.data;
    } else if (props.instance.product_type === 'custom') {
      const profiles = await listAll((q) => policiesApi.list('gpu-pricing', q));
      if (mine === pricingSeq) pricingProfiles.value = profiles;
    }
  } catch (error) {
    errorToast(error);
  } finally {
    if (mine === pricingSeq) pricingLoading.value = false;
  }
}
watch(() => props.instance.revision, loadPricing, { immediate: true });
async function savePricing() {
  if (!canSetPricing.value || !pricingId.value) return;
  const id = pricingId.value;
  const r = await pricingAction.run(
    (key) =>
      instancesApi.putGPUPricingPolicy(
        props.instance.id,
        { policy_version_id: id },
        { ifMatch: props.etag, key },
      ),
    { onConflict: () => emit('changed'), success: 'GPU 定价已绑定' },
  );
  if (r) emit('changed');
}

async function loadChoices() {
  try {
    const [g, n] = await Promise.all([
      listAll(catalogApi.gpuOptions, { node_id: props.instance.node_id }),
      listAll((q) => nodesApi.gpus(props.instance.node_id, q)),
    ]);
    gpuOptions.value = g;
    nodeGPUs.value = n;
  } catch (error) {
    errorToast(error);
  }
}

function uuidOptions(model: null | string | undefined, exclude: string[] = []) {
  return nodeGPUs.value
    .filter((g) => (!model || g.model === model) && !exclude.includes(g.uuid))
    .map((g) => ({
      disabled: g.status !== 'available',
      label: `${g.model} · ${g.uuid}（${enumLabel('gpuStatus', g.status).text}，已分配 ${g.allocation_count}/${g.max_allocations}）`,
      value: g.uuid,
    }));
}

/* ---------- 挂载 ---------- */

const attachOpen = ref(false);
const attach = ref({
  allow_live: false,
  count: 1,
  fixed_uuids: [] as string[],
  max_hourly_pt: '',
  option: undefined as string | undefined,
});
const attachAction = useAction();
const optionKey = (o: GPUOption) => `${o.model}\u0000${o.access_mode}`;
const attachOption = computed(() =>
  gpuOptions.value.find((o) => optionKey(o) === attach.value.option),
);
const optionChoices = computed(() => {
  const seen = new Set<string>();
  return gpuOptions.value
    .filter(
      (o) =>
        boundPricing.value &&
        o.model === boundPricing.value.config.gpu_model &&
        o.access_mode === boundPricing.value.config.access_mode,
    )
    .flatMap((o) => {
      const key = optionKey(o);
      if (seen.has(key)) return [];
      seen.add(key);
      return [
        {
          label: `${o.model} · ${ACCESS_MODE_NAMES[o.access_mode]} · ${formatPt(String(boundPricing.value!.config.gpu_hourly_pt))} pt/h · 显存 ${formatMib(o.memory_mib)} · ${o.availability === 'available' ? '有货' : '暂无'}`,
          value: key,
        },
      ];
    });
});

function openAttach() {
  attach.value = {
    allow_live: false,
    count: 1,
    fixed_uuids: [],
    max_hourly_pt: '',
    option: undefined,
  };
  attachAction.resetKey();
  void loadChoices();
  attachOpen.value = true;
}
watch(
  () => attach.value.option,
  () => {
    attach.value.max_hourly_pt = boundPricing.value
      ? String(boundPricing.value.config.gpu_hourly_pt)
      : '';
    attach.value.fixed_uuids = [];
  },
);
watch(attach, () => attachAction.resetKey(), { deep: true });

const attachProblem = computed(() => {
  const a = attach.value;
  if (missingPricing.value || !boundPricing.value)
    return '请先配置并加载实例 GPU 定价';
  if (!Number.isInteger(a.count) || a.count < 1) return '数量至少为 1';
  if (!attachOption.value) return '请选择 GPU 型号';
  if (!PT_PATTERN.test(a.max_hourly_pt))
    return '单价上限必须是非负数，最多 6 位小数';
  if (a.fixed_uuids.length > 0 && a.fixed_uuids.length !== a.count)
    return `指定 GPU 数量必须等于 ${a.count}`;
  if (running.value && !a.allow_live) return '实例运行中，必须勾选"运行中挂载"';
  return null;
});

async function submitAttach() {
  const opt = attachOption.value;
  if (attachProblem.value || !opt) return false;
  const a = attach.value;
  const body: AttachBody = {
    access_mode: opt.access_mode,
    allow_live: running.value && a.allow_live,
    count: a.count,
    max_hourly_pt: a.max_hourly_pt,
    model: opt.model,
  };
  if (a.fixed_uuids.length > 0) body.fixed_uuids = [...a.fixed_uuids];
  return done(
    await attachAction.run(
      (key) =>
        instancesApi.attachGPU(props.instance.id, body, {
          ifMatch: props.etag,
          key,
        }),
      {
        onConflict: () => emit('changed'),
        success: '已提交挂载',
      },
    ),
    '挂载 GPU',
  );
}

/* ---------- 移除 ---------- */

const detachOpen = ref(false);
const detachAction = useAction();
function openDetach() {
  detachAction.resetKey();
  detachOpen.value = true;
}
async function submitDetach() {
  const ids = activeSelected.value.map((a) => a.id);
  if (ids.length === 0) return false;
  return done(
    await detachAction.run(
      (key) =>
        instancesApi.detachGPU(
          props.instance.id,
          { allocation_ids: ids, allow_live: running.value },
          { ifMatch: props.etag, key },
        ),
      { onConflict: () => emit('changed'), success: '已提交移除' },
    ),
    '移除 GPU',
  );
}

/* ---------- 换卡 ---------- */

const rebindOpen = ref(false);
const rebind = ref({ fixed_uuids: [] as string[], max_hourly_pt: '' });
const rebindAction = useAction();
function openRebind() {
  const first = activeSelected.value[0];
  rebind.value = { fixed_uuids: [], max_hourly_pt: first?.gpu_hourly_pt ?? '' };
  rebindAction.resetKey();
  void loadChoices();
  rebindOpen.value = true;
}
watch(rebind, () => rebindAction.resetKey(), { deep: true });
const rebindProblem = computed(() => {
  const n = activeSelected.value.length;
  if (n === 0) return '请选择要更换的使用中分配';
  if (!PT_PATTERN.test(rebind.value.max_hourly_pt))
    return '单价上限必须是非负数，最多 6 位小数';
  if (
    rebind.value.fixed_uuids.length > 0 &&
    rebind.value.fixed_uuids.length !== n
  )
    return `指定 GPU 数量必须等于 ${n}`;
  return null;
});
async function submitRebind() {
  if (rebindProblem.value) return false;
  const body: Parameters<typeof instancesApi.rebindGPU>[1] = {
    allocation_ids: activeSelected.value.map((a) => a.id),
    allow_live: running.value,
    max_hourly_pt: rebind.value.max_hourly_pt,
  };
  if (rebind.value.fixed_uuids.length > 0)
    body.fixed_uuids = [...rebind.value.fixed_uuids];
  return done(
    await rebindAction.run(
      (key) =>
        instancesApi.rebindGPU(props.instance.id, body, {
          ifMatch: props.etag,
          key,
        }),
      {
        onConflict: () => emit('changed'),
        success: '已提交换卡',
      },
    ),
    '换卡',
  );
}

function done(
  r: Awaited<ReturnType<typeof instancesApi.attachGPU>> | undefined,
  label: string,
) {
  if (!r) return false;
  trackAccepted(r.data, label, () => {
    void loadAllocations();
    emit('changed');
  });
  selected.value = [];
  emit('changed');
  void loadAllocations();
  return true;
}
</script>

<template>
  <Card size="small">
    <Alert
      v-if="taskState"
      class="mb-3"
      :message="taskState.message"
      show-icon
      :type="taskState.type"
    >
      <template #action
        ><RouterLink :to="`/admin/tasks?instance_id=${instance.id}`"
          >查看任务</RouterLink
        ></template
      >
    </Alert>
    <Alert
      v-if="needsPricingSetup"
      class="mb-3"
      show-icon
      type="warning"
      message="此自定义实例尚未绑定 GPU 定价"
    >
      <template #description>
        <p>
          首次挂载前须显式选择定价；仅没有任何历史 GPU
          分配记录的实例可设置。保存后型号、访问方式与价格来自该绑定，不修改历史费率。
        </p>
        <Space wrap>
          <Select
            v-model:value="pricingId"
            class="min-w-72"
            :loading="pricingLoading"
            :options="pricingOptions"
            placeholder="选择 GPU 定价配置档"
          />
          <Button
            :disabled="!canSetPricing"
            :loading="pricingAction.pending.value"
            size="small"
            type="primary"
            @click="savePricing"
            >绑定定价</Button
          >
          <Button :loading="pricingLoading" size="small" @click="loadPricing"
            >刷新选项</Button
          >
        </Space>
      </template>
    </Alert>
    <div class="mb-3 flex flex-wrap items-center justify-between gap-2">
      <Space>
        <Button
          v-if="instance.capabilities.gpu_attach"
          :disabled="busy || missingPricing || !boundPricing"
          size="small"
          type="primary"
          @click="openAttach"
        >
          挂载 GPU
        </Button>
        <Button
          :disabled="busy || activeSelected.length === 0"
          size="small"
          @click="openDetach"
          >移除所选</Button
        >
        <Button
          v-if="instance.capabilities.gpu_rebind"
          :disabled="busy || activeSelected.length === 0"
          size="small"
          @click="openRebind"
        >
          换卡
        </Button>
      </Space>
      <label class="flex items-center gap-2 text-sm">
        <Switch v-model:checked="showHistory" size="small" /> 显示历史
      </label>
    </div>
    <Table
      :columns="columns"
      :data-source="rows"
      :loading="loading"
      :pagination="false"
      row-key="id"
      :row-selection="rowSelection"
      size="small"
    >
      <template #bodyCell="{ column, record }">
        <template v-if="column.key === 'uuid'">
          <Tooltip :title="record.uuid"
            ><span class="font-mono">{{ shortId(record.uuid) }}</span></Tooltip
          >
        </template>
        <template v-else-if="column.key === 'status'">
          <Space :size="4" wrap>
            <StatusTag kind="allocation" :value="record.status" />
            <Tag
              v-if="
                !record.release_verified &&
                (record.status === 'releasing' || record.status === 'unknown')
              "
              class="m-0"
              color="warning"
            >
              旧卡释放未确认
            </Tag>
          </Space>
        </template>
        <template v-else-if="column.key === 'flags'"
          >{{ yesNo(record.configured) }} /
          {{ yesNo(record.observed) }}</template
        >
        <template v-else-if="column.key === 'sm'">
          {{
            live[record.uuid]
              ? formatPercent(live[record.uuid]!.sm_utilization_percent)
              : '—'
          }}
        </template>
        <template v-else-if="column.key === 'mem'">
          <span v-if="live[record.uuid]" class="font-mono">
            {{
              live[record.uuid]!.used_memory_bytes == null
                ? '未知'
                : formatBytes(live[record.uuid]!.used_memory_bytes)
            }}
            /
            {{
              live[record.uuid]!.memory_total_bytes == null
                ? '未知'
                : formatBytes(live[record.uuid]!.memory_total_bytes)
            }}
          </span>
          <span v-else>—</span>
        </template>
        <template v-else-if="column.key === 'proc'">
          {{
            live[record.uuid]
              ? (live[record.uuid]!.process_count ?? '未知')
              : '—'
          }}
        </template>
        <template v-else-if="column.key === 'price'">
          <span class="font-mono"
            >{{ formatPt(record.gpu_hourly_pt) }} pt/h</span
          >
        </template>
        <template v-else-if="column.key === 'time'">
          <TimeText :value="record.created_at" />
          <template v-if="record.released_at">
            → <TimeText :value="record.released_at"
          /></template>
        </template>
      </template>
    </Table>
    <div class="text-muted-foreground mt-2 text-xs">
      运行中挂载、移除或换卡不会迁移 CUDA 上下文，GPU
      程序可能需要重新初始化。移除和换卡要求新鲜完整的空闲证据。
    </div>

    <ConfirmAction
      v-model:open="attachOpen"
      description="后端按最低负载选卡；指定 GPU 时仍做准入检查。"
      :on-confirm="submitAttach"
      title="挂载 GPU"
    >
      <Form layout="vertical">
        <div class="grid grid-cols-2 gap-x-3">
          <FormItem label="数量" required>
            <InputNumber
              v-model:value="attach.count"
              class="w-full"
              :min="1"
              :precision="0"
            />
          </FormItem>
          <FormItem label="单价上限（pt/h）" required>
            <Input v-model:value="attach.max_hourly_pt" class="font-mono" />
          </FormItem>
        </div>
        <FormItem label="型号与共享方式" required>
          <Select
            v-model:value="attach.option"
            :options="optionChoices"
            placeholder="选择 GPU"
          />
        </FormItem>
        <FormItem
          label="指定 GPU（可选）"
          :help="`不填由后端选择；填写时数量必须为 ${attach.count}`"
        >
          <Select
            v-model:value="attach.fixed_uuids"
            class="font-mono"
            :disabled="!attachOption"
            :max-count="attach.count"
            mode="multiple"
            :options="uuidOptions(attachOption?.model)"
            placeholder="自动选择"
          />
        </FormItem>
        <FormItem v-if="running">
          <Checkbox v-model:checked="attach.allow_live"
            >运行中挂载：CUDA 程序可能需要重新初始化</Checkbox
          >
        </FormItem>
      </Form>
      <Alert
        v-if="attachProblem"
        class="mb-3"
        :message="attachProblem"
        show-icon
        type="warning"
      />
    </ConfirmAction>

    <ConfirmAction
      v-model:open="detachOpen"
      danger
      :description="`移除 ${activeSelected.length} 张 GPU。GPU 仍有进程或显存占用时会被拒绝，请先结束程序或停止实例。`"
      :on-confirm="submitDetach"
      :risk-text="running ? '运行中移除：CUDA 程序可能需要重新初始化' : ''"
      title="移除 GPU"
    >
      <div class="mb-3 font-mono text-xs">
        <div v-for="a in activeSelected" :key="a.id">
          {{ a.model }} · {{ a.uuid }}
        </div>
      </div>
    </ConfirmAction>

    <ConfirmAction
      v-model:open="rebindOpen"
      description="为所选分配更换 GPU。运行中换卡不会迁移 CUDA 上下文，程序可能需要重新初始化。"
      :on-confirm="submitRebind"
      :risk-text="running ? '运行中换卡：CUDA 程序可能需要重新初始化' : ''"
      title="换卡"
    >
      <div class="mb-3 font-mono text-xs">
        <div v-for="a in activeSelected" :key="a.id">
          {{ a.model }} · {{ a.uuid }}
        </div>
      </div>
      <Form layout="vertical">
        <FormItem label="单价上限（pt/h）" required>
          <Input v-model:value="rebind.max_hourly_pt" class="font-mono" />
        </FormItem>
        <FormItem
          label="指定目标 GPU（可选）"
          :help="`不填由后端选择；填写时数量必须为 ${activeSelected.length}`"
        >
          <Select
            v-model:value="rebind.fixed_uuids"
            class="font-mono"
            :max-count="activeSelected.length"
            mode="multiple"
            :options="
              uuidOptions(
                activeSelected[0]?.model,
                activeSelected.map((a) => a.uuid),
              )
            "
            placeholder="自动选择"
          />
        </FormItem>
      </Form>
      <Alert
        v-if="rebindProblem"
        class="mb-3"
        :message="rebindProblem"
        show-icon
        type="warning"
      />
    </ConfirmAction>
  </Card>
</template>
