<script lang="ts" setup>
import type { EChartsOption } from 'echarts';
import type { EchartsUIType } from '@vben/plugins/echarts';
import type { HistoryPoint, LatestGPUMetric, LatestMetrics, MetricQuality, MetricsHistory } from '#/api/types';
import type { ErrorText } from '#/locales/errors';

import { computed, onBeforeUnmount, ref, shallowRef, watch } from 'vue';
import { EchartsUI, useEcharts } from '@vben/plugins/echarts';
import { useAccessStore } from '@vben/stores';
import { Alert, Button, Card, Empty, Select, Spin, Tag } from 'ant-design-vue';
import { MarkAreaComponent } from 'echarts/components';
import * as echarts from 'echarts/core';

import { memberApi } from '#/api/backend';
import { ApiError } from '#/api/request';
import ErrorFeedback from '#/components/error-feedback.vue';
import TimeText from '#/components/time-text.vue';
import { usePolling } from '#/composables/api';
import { QUALITY_REASONS } from '#/locales/enums';
import { describeError } from '#/locales/errors';
import { useAuthStore } from '#/store/auth';
import { formatBytes, formatPercent, shortId, toShanghaiISO } from '#/utils/format';

echarts.use([MarkAreaComponent]);
const props = defineProps<{ instanceId: string }>();
const auth = useAuthStore();
const access = useAccessStore();
const identity = computed(() => !access.loginExpired && auth.me?.user.role === 'user' ? auth.me.user.id : null);
const RANGES = [
  { label: '1 小时', value: 1 }, { label: '6 小时', value: 6 },
  { label: '24 小时', value: 24 }, { label: '7 天', value: 168 },
  { label: '30 天', value: 720 }, { label: '自定义', value: 'custom' },
];
// 与管理端相同的 auto 分辨率和逐页取数；不把达到上限的部分结果伪装成完整曲线。
const MAX_PAGES = 10;
const PAGE_SIZE = 1000;
const GIB = 1024 ** 3;
type Pair = [number, null | number];
const num = (value: null | number | string | undefined): null | number => {
  if (value === null || value === undefined || value === '') return null;
  const parsed = Number(value);
  return Number.isFinite(parsed) ? parsed : null;
};
const bytesText = (value: null | string | undefined) => value == null ? '未知' : formatBytes(value);
const qualityReasons = (quality: MetricQuality) => {
  const reasons = new Set(quality.reasons);
  if (quality.error_code) reasons.add(quality.error_code);
  return [...reasons];
};
const normalReason = (reason: string) => reason === 'gpu_utilization_no_samples' || reason === 'instance_not_running';
const gpuHasKnownActivity = (gpu: LatestGPUMetric) => gpu.process_count !== null && gpu.referencing_process_count !== null && gpu.used_memory_bytes !== null
  && (gpu.sm_utilization_percent !== null || gpu.quality.reasons.includes('gpu_utilization_no_samples'));
const normalSampling = computed(() => {
  const data = latest.value;
  if (!data || !data.collection_complete) return false;
  const qualities = [data.quality, ...data.gpus.map((gpu) => gpu.quality)];
  const reasons = qualities.flatMap(qualityReasons);
  const stopped = data.instance_state === 'stopped' && reasons.includes('instance_not_running');
  return reasons.length > 0 && reasons.every(normalReason)
    && qualities.every((quality) => stopped || (quality.attribution_complete && quality.reference_scan_quality === 'complete'))
    && (stopped || (data.instance_state === 'running' && data.gpus.every((gpu) => gpuHasKnownActivity(gpu)
      && (!gpu.quality.reasons.includes('gpu_utilization_no_samples') || gpu.process_count === 0))));
});
const range = ref<number | string>(1);
const customFrom = ref(toShanghaiISO(new Date(Date.now() - 3600_000)).slice(0, 16));
const customTo = ref(toShanghaiISO(new Date()).slice(0, 16));
const rangeError = computed(() => {
  if (range.value !== 'custom') return '';
  const from = Date.parse(`${customFrom.value}:00+08:00`);
  const to = Date.parse(`${customTo.value}:00+08:00`);
  return !Number.isFinite(from) || !Number.isFinite(to) || from >= to ? '请选择有效的开始和结束时间，结束时间须晚于开始时间。' : '';
});
const latest = shallowRef<LatestMetrics | null>(null);
const latestError = shallowRef<ErrorText | null>(null);
const latestLoading = ref(false);
const history = shallowRef<MetricsHistory | null>(null);
const points = shallowRef<HistoryPoint[]>([]);
const historyError = shallowRef<ErrorText | null>(null);
const historyLoading = ref(false);
const truncated = ref(false);
const selectedGPU = ref<string>();
let generation = 0;
let historySeq = 0;
let disposed = false;
let lastHistoryLoad = 0;

async function loadLatest() {
  if (disposed || latestLoading.value || !identity.value || !props.instanceId) return;
  const token = generation;
  const id = props.instanceId;
  const user = identity.value;
  const current = () => !disposed && token === generation && id === props.instanceId && user === identity.value;
  latestLoading.value = true;
  try {
    const response = await memberApi.latest(id);
    if (!current()) return;
    if (response.data.instance_id !== id) throw new ApiError(403, { code: 'forbidden', details: null, message: '无权查看此实例监控' }, response.requestId, null);
    latest.value = response.data;
    latestError.value = null;
  } catch (error) {
    if (!current()) return;
    latest.value = null;
    latestError.value = describeError(error);
  } finally {
    if (current()) latestLoading.value = false;
  }
}

const retainedHistoryGPUs = shallowRef<Record<string, string>>({});
async function loadHistory() {
  const mine = ++historySeq;
  history.value = null;
  points.value = [];
  historyError.value = null;
  truncated.value = false;
  historyLoading.value = false;
  if (disposed || !identity.value || !props.instanceId || rangeError.value) return;
  const token = generation;
  const id = props.instanceId;
  const user = identity.value;
  const current = () => !disposed && token === generation && mine === historySeq && id === props.instanceId && user === identity.value;
  const to = new Date();
  const base = range.value === 'custom'
    ? { from: `${customFrom.value}:00+08:00`, to: `${customTo.value}:00+08:00` }
    : { from: toShanghaiISO(new Date(to.getTime() - Number(range.value) * 3600_000)), to: toShanghaiISO(to) };
  historyLoading.value = true;
  lastHistoryLoad = Date.now();
  try {
    const items: HistoryPoint[] = [];
    let metadata: MetricsHistory | null = null;
    let cursor: string | undefined;
    const seen = new Set<string>();
    for (let page = 0; page < MAX_PAGES; page++) {
      const response = await memberApi.history(id, { ...base, cursor, limit: PAGE_SIZE, resolution: 'auto' });
      if (!current()) return;
      if (!metadata) metadata = { ...response.data, collection_gaps: [], gaps: [] };
      metadata.gaps.push(...response.data.gaps);
      metadata.collection_gaps.push(...response.data.collection_gaps);
      items.push(...response.data.items);
      cursor = response.data.next_cursor ?? undefined;
      if (!cursor) break;
      if (seen.has(cursor)) break;
      seen.add(cursor);
    }
    const retained = { ...retainedHistoryGPUs.value };
    for (const point of items) {
      for (const gpu of point.gpus ?? []) {
        const uuid = gpu.uuid ?? gpu.gpu_uuid;
        if (uuid) retained[uuid] = gpu.gpu_model ?? retained[uuid] ?? 'GPU';
      }
    }
    retainedHistoryGPUs.value = retained;
    history.value = metadata;
    points.value = items;
    truncated.value = !!cursor;
  } catch (error) {
    if (!current()) return;
    historyError.value = describeError(error);
  } finally {
    if (current()) historyLoading.value = false;
  }
}

const gpuOptions = computed(() => {
  const cards = new Map<string, string>();
  for (const gpu of latest.value?.gpus ?? []) cards.set(gpu.uuid, `${gpu.model ?? 'GPU'} · ${shortId(gpu.uuid)}`);
  for (const [id, model] of Object.entries(retainedHistoryGPUs.value)) {
    if (!cards.has(id)) cards.set(id, `${model} · ${shortId(id)}（历史）`);
  }
  return [...cards].map(([value, label]) => ({ label, value }));
});
watch(gpuOptions, (options) => {
  if (!options.some((option) => option.value === selectedGPU.value)) selectedGPU.value = options[0]?.value;
}, { flush: 'sync' });
const selectedLatest = computed(() => latest.value?.gpus.find((gpu) => gpu.uuid === selectedGPU.value));
const gapIntervals = computed(() => {
  const metadata = history.value;
  if (!metadata) return [];
  const ranges = [...metadata.gaps.map((gap) => [gap.from, gap.to]), ...metadata.collection_gaps.map((gap) => [gap.from_at, gap.to_at])];
  const unique = new Map<string, [number, number]>();
  for (const [from, to] of ranges) {
    const start = Date.parse(from ?? '');
    const end = Date.parse(to ?? '');
    if (Number.isFinite(start) && Number.isFinite(end) && start < end) unique.set(`${start}:${end}`, [start, end]);
  }
  return [...unique.values()];
});
const series = computed(() => {
  const raw = history.value?.resolution === 'raw';
  const cpu: Pair[] = [];
  const memory: Pair[] = [];
  const gpuSM: Pair[] = [];
  const gpuMemory: Pair[] = [];
  for (const point of points.value) {
    const time = Date.parse((raw ? point.sampled_at : point.bucket_start_at) ?? '');
    if (!Number.isFinite(time)) continue;
    const gpu = point.gpus?.find((item) => (raw ? item.uuid : item.gpu_uuid) === selectedGPU.value);
    const mem = num(raw ? point.memory?.used_bytes : point.memory_used_avg_bytes);
    const vram = num(raw ? gpu?.used_memory_bytes : gpu?.memory_used_avg_bytes);
    cpu.push([time, num(raw ? point.cpu?.allocation_percent : point.cpu_utilization_avg_pct)]);
    memory.push([time, mem === null ? null : mem / GIB]);
    gpuSM.push([time, num(raw ? gpu?.sm_utilization_percent : gpu?.utilization_avg_pct)]);
    gpuMemory.push([time, vram === null ? null : vram / GIB]);
  }
  // 缺测不以直线跨越，也不填 0；橙色区域另外标出服务端确认的缺口。
  for (const data of [cpu, memory, gpuSM, gpuMemory]) {
    for (const [start, end] of gapIntervals.value) data.push([start, null], [end, null]);
    data.sort((a, b) => a[0] - b[0]);
  }
  return { cpu, gpuMemory, gpuSM, memory };
});
const cpuChart = ref<EchartsUIType>();
const memoryChart = ref<EchartsUIType>();
const gpuSMChart = ref<EchartsUIType>();
const gpuMemoryChart = ref<EchartsUIType>();
const cpu = useEcharts(cpuChart);
const memory = useEcharts(memoryChart);
const gpuSM = useEcharts(gpuSMChart);
const gpuMemory = useEcharts(gpuMemoryChart);
function shanghaiLabel(ms: number) {
  const iso = toShanghaiISO(new Date(ms));
  return `${iso.slice(5, 10)} ${iso.slice(11, 16)}`;
}
function option(name: string, data: Pair[], percent: boolean): EChartsOption {
  return {
    animation: false,
    grid: { bottom: 30, containLabel: true, left: 8, right: 12, top: 16 },
    tooltip: { trigger: 'axis', valueFormatter: (value) => typeof value === 'number' ? (percent ? formatPercent(value) : `${Math.round(value * 100) / 100} GiB`) : '未知' },
    xAxis: { type: 'time', min: Date.parse(history.value?.from ?? ''), max: Date.parse(history.value?.to ?? ''), axisLabel: { formatter: shanghaiLabel, hideOverlap: true } },
    yAxis: { type: 'value', min: 0, axisLabel: { formatter: (value: number) => `${value}${percent ? '%' : ' GiB'}` } },
    series: [{ name, type: 'line', data, showSymbol: false, connectNulls: false, markArea: {
      silent: true, itemStyle: { color: 'rgba(250, 140, 22, 0.15)' },
      data: gapIntervals.value.map(([start, end]) => [{ xAxis: start }, { xAxis: end }]),
    } }],
  };
}
watch([series, cpuChart, memoryChart, gpuSMChart, gpuMemoryChart], () => {
  if (disposed || !history.value || points.value.length === 0) return;
  void cpu.renderEcharts(option('CPU 相对配额', series.value.cpu, true));
  void memory.renderEcharts(option('内存已用', series.value.memory, false));
  if (selectedGPU.value && gpuSMChart.value && gpuMemoryChart.value) {
    void gpuSM.renderEcharts(option('所选 GPU SM 利用率', series.value.gpuSM, true));
    void gpuMemory.renderEcharts(option('所选 GPU 归因显存', series.value.gpuMemory, false));
  }
}, { flush: 'post' });
watch([identity, () => props.instanceId], () => {
  ++generation;
  ++historySeq;
  latest.value = null;
  latestError.value = null;
  retainedHistoryGPUs.value = {};
  latestLoading.value = false;
  selectedGPU.value = undefined;
  void loadLatest();
  void loadHistory();
}, { immediate: true, flush: 'sync' });
watch([range, customFrom, customTo], () => void loadHistory(), { flush: 'sync' });
usePolling(async () => {
  await loadLatest();
  if (range.value !== 'custom' && !historyLoading.value && Date.now() - lastHistoryLoad >= 60_000) await loadHistory();
}, () => 10_000);
onBeforeUnmount(() => { disposed = true; ++generation; ++historySeq; });
</script>

<template>
  <div class="metrics-panel min-w-0 space-y-3">
    <Alert v-if="!identity" message="请使用普通用户账号登录后查看监控。" show-icon type="info" />
    <template v-else>
      <Card size="small" title="最新采样">
        <ErrorFeedback v-if="latestError" :feedback="latestError" />
        <template v-else-if="latest">
          <div class="mb-3 flex flex-wrap items-center gap-2 text-sm">
            <span>采样时间：<TimeText :value="latest.sampled_at" /></span>
            <Tag :color="latest.stale ? 'warning' : 'success'">{{ latest.stale ? '已过期' : '未过期' }}</Tag>
            <span>采样距今 {{ num(latest.age_seconds) ?? '未知' }} 秒</span>
          </div>
          <Alert v-if="latest.instance_state === 'stopped'" class="mb-3" message="实例已停止，没有运行态 GPU 占用采样。未知不表示占用为 0；历史监控仍可查看。" show-icon type="info" />
          <div class="grid gap-3 sm:grid-cols-2">
            <div><div class="text-muted-foreground text-xs">CPU 使用率（相对配额，不限制为 100%）</div><strong>{{ formatPercent(latest.cpu.allocation_percent) }}</strong></div>
            <div><div class="text-muted-foreground text-xs">内存已用 / 限额</div><strong>{{ bytesText(latest.memory.used_bytes) }} / {{ bytesText(latest.memory.limit_bytes) }}</strong></div>
          </div>
          <div v-if="!normalSampling || qualityReasons(latest.quality).some((reason) => reason !== 'gpu_utilization_no_samples')" class="mt-3 flex flex-wrap items-center gap-1 text-xs">
            <span v-if="!normalSampling">采集{{ latest.collection_complete ? '完整' : '不完整' }}；质量{{ latest.quality.complete ? '完整' : '不完整' }}；归因{{ latest.quality.attribution_complete ? '完整' : '不完整' }}</span>
            <Tag v-if="!normalSampling && (!latest.collection_complete || !latest.quality.complete || !latest.quality.attribution_complete || latest.quality.reference_scan_quality !== 'complete')" color="warning">部分指标不可用或归因不完整</Tag>
            <Tag v-for="reason in qualityReasons(latest.quality).filter((reason) => reason !== 'gpu_utilization_no_samples')" :key="reason" :color="normalReason(reason) ? undefined : 'warning'">{{ QUALITY_REASONS[reason] ?? reason }}</Tag>
          </div>
        </template>
        <Spin v-else-if="latestLoading" :spinning="true" />
        <p v-else class="text-muted-foreground text-sm">暂无最新采样，当前 GPU 占用未知。</p>
      </Card>

      <Card size="small" title="历史监控">
        <div class="mb-3 flex flex-wrap items-end gap-3">
          <label class="min-w-0"><span class="mb-1 block text-xs">时间范围</span><Select v-model:value="range" aria-label="监控时间范围" :options="RANGES" class="w-36" /></label>
          <template v-if="range === 'custom'">
            <label class="time-field"><span>开始时间（+08:00）</span><input v-model="customFrom" type="datetime-local" /></label>
            <label class="time-field"><span>结束时间（+08:00）</span><input v-model="customTo" type="datetime-local" /></label>
          </template>
          <Button :disabled="!!rangeError" :loading="historyLoading" @click="loadHistory">刷新曲线</Button>
          <span class="text-muted-foreground text-xs">自动分辨率 · 最新值每 10 秒更新 · 相对范围曲线每分钟更新</span>
        </div>
        <Alert v-if="rangeError" class="mb-3" :message="rangeError" show-icon type="warning" />
        <ErrorFeedback v-if="historyError" class="mb-3" :feedback="historyError" />
        <Spin :spinning="historyLoading">
          <Alert v-if="truncated" class="mb-3" :message="`仅展示部分数据：已达到最多 ${MAX_PAGES} 页（每页 ${PAGE_SIZE} 点）的取数上限或服务端游标重复。请缩小时间范围后重新查询。`" show-icon type="warning" />
          <Empty v-if="!historyLoading && !historyError && !rangeError && points.length === 0" description="该时段没有监控数据（不代表使用量为 0）" />
          <div v-if="points.length" class="grid min-w-0 gap-3 xl:grid-cols-2">
            <section class="min-w-0"><h3 class="text-sm font-medium">CPU 相对配额（%）</h3><EchartsUI ref="cpuChart" height="250px" /></section>
            <section class="min-w-0"><h3 class="text-sm font-medium">内存已用（GiB）</h3><EchartsUI ref="memoryChart" height="250px" /></section>
          </div>
          <section class="mt-3 min-w-0">
            <label v-if="gpuOptions.length" class="mb-3 block min-w-0 text-sm">选择 GPU（含历史卡）<Select v-model:value="selectedGPU" class="mt-1 w-full" aria-label="选择监控 GPU" :options="gpuOptions" /></label>
            <Empty v-else-if="!latestLoading && !historyLoading && latest && history && !latestError && !historyError && !rangeError" description="当前采样和所选历史时段均无 GPU 数据（不代表未挂载 GPU）" />
            <p v-else-if="!gpuOptions.length" class="text-muted-foreground mb-3 text-xs">GPU 数据尚未完整取得，不能据此判断是否挂载 GPU 或当前占用。</p>
            <div v-if="selectedLatest" class="mb-3 flex flex-wrap gap-2 text-xs">
              <span class="break-all">UUID：{{ selectedLatest.uuid }}</span>
              <span>SM：{{ selectedLatest.sm_utilization_percent === null && selectedLatest.quality.reasons.includes('gpu_utilization_no_samples') ? '暂无活动采样' : formatPercent(selectedLatest.sm_utilization_percent) }}</span>
              <span>归因显存：{{ bytesText(selectedLatest.used_memory_bytes) }}</span>
              <span>GPU 进程数：{{ selectedLatest.process_count ?? '未知' }}</span>
              <span>设备引用数：{{ selectedLatest.referencing_process_count ?? '未知' }}</span>
              <span>已配置：{{ selectedLatest.configured ? '是' : '否' }} · 已观测：{{ selectedLatest.observed ? '是' : '否' }}<template v-if="!normalSampling"> · 归因{{ selectedLatest.quality.attribution_complete ? '完整' : '不完整' }}</template></span>
              <Tag v-if="!normalSampling && (!selectedLatest.quality.complete || !selectedLatest.quality.attribution_complete || selectedLatest.quality.reference_scan_quality !== 'complete')" color="warning">部分 GPU 指标不可用或归因不完整</Tag>
              <Tag v-if="latest?.instance_state !== 'stopped' && !gpuHasKnownActivity(selectedLatest)" color="warning">部分 GPU 活动指标未知</Tag>
              <Tag v-for="reason in qualityReasons(selectedLatest.quality).filter((reason) => reason !== 'gpu_utilization_no_samples')" :key="reason" :color="normalReason(reason) ? undefined : 'warning'">{{ QUALITY_REASONS[reason] ?? reason }}</Tag>
            </div>
            <p v-else-if="selectedGPU && !latest" class="text-muted-foreground mb-3 text-xs">暂无最新 GPU 采样，当前占用未知；以下仅显示所选 UUID 的历史数据。</p>
            <p v-else-if="selectedGPU" class="text-muted-foreground mb-3 text-xs">所选 GPU 在最新采样中不存在；以下仅显示该 UUID 的历史数据。</p>
            <div v-if="selectedGPU && points.length" class="grid min-w-0 gap-3 xl:grid-cols-2">
              <section class="min-w-0"><h3 class="text-sm font-medium">所选 GPU SM 利用率（%）</h3><EchartsUI ref="gpuSMChart" height="250px" /></section>
              <section class="min-w-0"><h3 class="text-sm font-medium">所选 GPU 归因显存（GiB）</h3><EchartsUI ref="gpuMemoryChart" height="250px" /></section>
            </div>
          </section>
        </Spin>
      </Card>
    </template>
  </div>
</template>

<style scoped>
.metrics-panel { overflow-wrap: anywhere; }
.time-field { display: flex; min-width: 0; flex-direction: column; gap: 4px; font-size: 12px; }
.time-field input { max-width: 100%; min-height: 32px; border: 1px solid hsl(var(--border)); border-radius: 6px; padding: 4px 8px; background: hsl(var(--background)); color: inherit; }
.time-field input:focus-visible { outline: 2px solid hsl(var(--primary)); outline-offset: 2px; }
@media (max-width: 520px) { .time-field { width: 100%; } .time-field input { width: 100%; } .metrics-panel :deep(.ant-card-body) { padding: 12px; } }
</style>
