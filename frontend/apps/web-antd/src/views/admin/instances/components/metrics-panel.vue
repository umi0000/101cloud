<script lang="ts" setup>
/** 监控标签页：最新值每 10 秒刷新；历史曲线按时间范围取数，null 断线，缺测区域画 markArea。 */
import type { EChartsOption } from 'echarts';

import type { EchartsUIType } from '@vben/plugins/echarts';

import type { HistoryPoint, LatestMetrics, MetricsHistory } from '#/api/types';
import type { ErrorText } from '#/locales/errors';

import { computed, onMounted, ref, shallowRef, watch } from 'vue';

import { EchartsUI, useEcharts } from '@vben/plugins/echarts';

import { Alert, Card, Descriptions, DescriptionsItem, Empty, RadioButton, RadioGroup, Spin, Table, Tag, Tooltip } from 'ant-design-vue';
import { MarkAreaComponent } from 'echarts/components';
import * as echarts from 'echarts/core';

import { instancesApi } from '#/api/backend';
import ErrorFeedback from '#/components/error-feedback.vue';
import TimeText from '#/components/time-text.vue';
import { usePolling } from '#/composables/api';
import { QUALITY_REASONS } from '#/locales/enums';
import { describeError } from '#/locales/errors';
import { formatBytes, formatPercent, shortId, toShanghaiISO } from '#/utils/format';

import { GPU_STATE_NAMES, yesNo } from './shared';

echarts.use([MarkAreaComponent]);

const props = defineProps<{ instanceId: string }>();

/* ---------- 最新值 ---------- */

const latest = shallowRef<LatestMetrics | null>(null);
const latestError = ref<ErrorText | null>(null);

async function loadLatest() {
  try {
    latest.value = (await instancesApi.latest(props.instanceId)).data;
    latestError.value = null;
  } catch (error) {
    latest.value = null;
    latestError.value = describeError(error);
  }
}
usePolling(loadLatest, () => 10_000);

const gpuColumns = [
  { key: 'model', title: '型号' },
  { key: 'uuid', title: 'UUID' },
  { key: 'sm', title: 'SM 利用率' },
  { key: 'mem', title: '显存' },
  { dataIndex: 'process_count', key: 'proc', title: '进程数' },
  { key: 'state', title: '状态' },
  { key: 'flags', title: '已配置 / 已观测' },
  { key: 'reasons', title: '质量' },
];

/* ---------- 历史 ---------- */

const RANGES = [
  { hours: 1, label: '1 小时' },
  { hours: 6, label: '6 小时' },
  { hours: 24, label: '24 小时' },
  { hours: 24 * 7, label: '7 天' },
  { hours: 24 * 30, label: '30 天' },
];
const MAX_PAGES = 10;

const rangeHours = ref(1);
const history = shallowRef<MetricsHistory | null>(null);
const points = shallowRef<HistoryPoint[]>([]);
const truncated = ref(false);
const historyLoading = ref(false);
const historyError = ref<ErrorText | null>(null);
let historySeq = 0;

async function loadHistory() {
  const mine = ++historySeq;
  historyLoading.value = true;
  historyError.value = null;
  const to = new Date();
  const from = new Date(to.getTime() - rangeHours.value * 3600_000);
  const base = { from: toShanghaiISO(from), limit: 1000, resolution: 'auto', to: toShanghaiISO(to) };
  try {
    const items: HistoryPoint[] = [];
    let first: MetricsHistory | null = null;
    let cursor: string | undefined;
    let pages = 0;
    let more = false;
    do {
      const r = await instancesApi.history(props.instanceId, { ...base, cursor });
      if (mine !== historySeq) return;
      first ??= r.data;
      items.push(...r.data.items);
      cursor = r.data.next_cursor ?? undefined;
      pages++;
      more = !!cursor;
    } while (more && pages < MAX_PAGES);
    history.value = first;
    points.value = items;
    truncated.value = more;
  } catch (error) {
    if (mine !== historySeq) return;
    history.value = null;
    points.value = [];
    historyError.value = describeError(error);
  } finally {
    if (mine === historySeq) historyLoading.value = false;
  }
}

const num = (v: null | number | string | undefined): null | number => {
  if (v === null || v === undefined || v === '') return null;
  const n = Number(v);
  return Number.isFinite(n) ? n : null;
};
const GIB = 1024 ** 3;

type Pair = [number, null | number];

const series = computed(() => {
  const raw = history.value?.resolution === 'raw';
  const cpu: Pair[] = [];
  const mem: Pair[] = [];
  const gpus = new Map<string, Pair[]>();
  const times: number[] = [];
  for (const p of points.value) {
    const t = Date.parse((raw ? p.sampled_at : p.bucket_start_at) ?? '');
    if (Number.isNaN(t)) continue;
    times.push(t);
    const memBytes = num(raw ? p.memory?.used_bytes : p.memory_used_avg_bytes);
    cpu.push([t, num(raw ? p.cpu?.allocation_percent : p.cpu_utilization_avg_pct)]);
    mem.push([t, memBytes === null ? null : memBytes / GIB]);
    for (const g of p.gpus ?? []) {
      const id = (raw ? g.uuid : g.gpu_uuid) ?? '';
      if (!id) continue;
      if (!gpus.has(id)) gpus.set(id, []);
    }
  }
  // 每张卡按时间对齐：该点没有这张卡时补 null 断线，不补 0。
  for (const [id, data] of gpus) {
    for (const [i, p] of points.value.entries()) {
      const t = times[i];
      if (t === undefined) continue;
      const g = (p.gpus ?? []).find((x) => (raw ? x.uuid : x.gpu_uuid) === id);
      data.push([t, g ? num(raw ? g.sm_utilization_percent : g.utilization_avg_pct) : null]);
    }
  }
  return { cpu, gpus, mem };
});

const gapAreas = computed(() => {
  const h = history.value;
  if (!h) return [];
  const ranges = [
    ...h.gaps.map((g) => [g.from, g.to] as const),
    ...h.collection_gaps.map((g) => [g.from_at, g.to_at] as const),
  ];
  return ranges
    .map(([a, b]) => [Date.parse(a), Date.parse(b)] as const)
    .filter(([a, b]) => !Number.isNaN(a) && !Number.isNaN(b))
    .map(([a, b]): [{ xAxis: number }, { xAxis: number }] => [{ xAxis: a }, { xAxis: b }]);
});

const gapReasons = computed(() => {
  const h = history.value;
  if (!h) return [];
  return [...new Set([...h.gaps.map((g) => g.reason), ...h.collection_gaps.map((g) => g.reason)])].filter(Boolean);
});

function shanghaiLabel(ms: number): string {
  const d = new Date(ms + 8 * 3600_000);
  const p = (n: number) => String(n).padStart(2, '0');
  const hm = `${p(d.getUTCHours())}:${p(d.getUTCMinutes())}`;
  return rangeHours.value > 24 ? `${p(d.getUTCMonth() + 1)}-${p(d.getUTCDate())} ${hm}` : hm;
}

function baseOption(yFormatter: (v: number) => string, valueText: (v: number) => string): EChartsOption {
  return {
    grid: { bottom: 30, containLabel: true, left: 10, right: 20, top: 40 },
    legend: { top: 0, type: 'scroll' },
    tooltip: {
      trigger: 'axis',
      valueFormatter: (v) => (typeof v === 'number' ? valueText(v) : '未知'),
    },
    xAxis: {
      axisLabel: { formatter: (v: number) => shanghaiLabel(v) },
      axisPointer: { label: { formatter: (p) => shanghaiLabel(Number(p.value)) } },
      type: 'time',
    },
    yAxis: { axisLabel: { formatter: (v: number) => yFormatter(v) }, min: 0, type: 'value' },
  };
}

const markArea = computed(() => ({
  data: gapAreas.value,
  itemStyle: { color: 'rgba(250, 140, 22, 0.15)' },
  silent: true,
}));

const usageChart = ref<EchartsUIType>();
const memoryChart = ref<EchartsUIType>();
const usage = useEcharts(usageChart);
const memory = useEcharts(memoryChart);

function render() {
  const s = series.value;
  void usage.renderEcharts({
    ...baseOption((v) => `${v}%`, (v) => formatPercent(v)),
    series: [
      { connectNulls: false, data: s.cpu, markArea: markArea.value, name: 'CPU', showSymbol: false, type: 'line' },
      ...[...s.gpus].map(([id, data]) => ({
        connectNulls: false,
        data,
        name: `GPU ${shortId(id, 8, 4)}`,
        showSymbol: false,
        type: 'line' as const,
      })),
    ],
  });
  void memory.renderEcharts({
    ...baseOption((v) => `${v} GiB`, (v) => `${Math.round(v * 100) / 100} GiB`),
    series: [{ connectNulls: false, data: s.mem, markArea: markArea.value, name: '内存已用', showSymbol: false, type: 'line' }],
  });
}

watch(series, () => {
  if (points.value.length > 0) render();
});
watch(rangeHours, () => void loadHistory());

onMounted(() => {
  void loadLatest();
  void loadHistory();
});

const coverageText = computed(() => {
  const f = num(history.value?.coverage.fraction);
  return f === null ? '未知' : formatPercent(f * 100);
});
const RESOLUTION_NAMES: Record<string, string> = { day: '按天', hour: '按小时', minute: '按分钟', raw: '原始采样' };
</script>

<template>
  <div class="flex flex-col gap-3">
    <Card size="small" title="最新值">
      <Alert v-if="latestError" :message="latestError.text" show-icon type="info">
        <template #description><ErrorFeedback :feedback="latestError" hide-message /></template>
      </Alert>
      <template v-else-if="latest">
        <div v-if="latest.stale" class="mb-2 text-sm text-orange-500">
          数据已过期（{{ Math.round(Number(latest.age_seconds)) }} 秒前）
        </div>
        <Descriptions :column="3" size="small">
          <DescriptionsItem label="CPU 使用率（相对已分配）">{{ formatPercent(latest.cpu.allocation_percent) }}</DescriptionsItem>
          <DescriptionsItem label="内存">
            <span class="font-mono">
              {{ latest.memory.used_bytes === null ? '未知' : formatBytes(latest.memory.used_bytes) }} /
              {{ latest.memory.limit_bytes === null ? '未知' : formatBytes(latest.memory.limit_bytes) }}
            </span>
          </DescriptionsItem>
          <DescriptionsItem label="采样时间"><TimeText :value="latest.sampled_at" /></DescriptionsItem>
        </Descriptions>
        <div v-if="latest.quality.reasons.length > 0" class="mb-2 flex flex-wrap items-center gap-1 text-sm">
          <span class="text-muted-foreground">质量：</span>
          <Tooltip v-for="r in latest.quality.reasons" :key="r" :title="QUALITY_REASONS[r] ?? r">
            <Tag class="m-0" color="warning">{{ QUALITY_REASONS[r] ?? r }}</Tag>
          </Tooltip>
        </div>
        <Table
          v-if="latest.gpus.length > 0"
          :columns="gpuColumns"
          :data-source="latest.gpus"
          :pagination="false"
          row-key="uuid"
          size="small"
        >
          <template #bodyCell="{ column, record }">
            <template v-if="column.key === 'model'">{{ record.model ?? '未知' }}</template>
            <template v-else-if="column.key === 'uuid'">
              <Tooltip :title="record.uuid"><span class="font-mono">{{ shortId(record.uuid) }}</span></Tooltip>
            </template>
            <template v-else-if="column.key === 'sm'">{{ formatPercent(record.sm_utilization_percent) }}</template>
            <template v-else-if="column.key === 'mem'">
              <span class="font-mono">
                {{ record.used_memory_bytes == null ? '未知' : formatBytes(record.used_memory_bytes) }} /
                {{ record.memory_total_bytes == null ? '未知' : formatBytes(record.memory_total_bytes) }}
              </span>
            </template>
            <template v-else-if="column.key === 'proc'">{{ record.process_count ?? '未知' }}</template>
            <template v-else-if="column.key === 'state'">{{ GPU_STATE_NAMES[record.state] ?? record.state }}</template>
            <template v-else-if="column.key === 'flags'">{{ yesNo(record.configured) }} / {{ yesNo(record.observed) }}</template>
            <template v-else-if="column.key === 'reasons'">
              <Tooltip v-for="r in record.quality.reasons" :key="r" :title="QUALITY_REASONS[r] ?? r">
                <Tag class="m-0 mr-1" color="warning">{{ QUALITY_REASONS[r] ?? r }}</Tag>
              </Tooltip>
              <span v-if="record.quality.reasons.length === 0" class="text-muted-foreground">—</span>
            </template>
          </template>
        </Table>
      </template>
      <Spin v-else />
    </Card>

    <Card size="small" title="历史">
      <template #extra>
        <RadioGroup v-model:value="rangeHours" button-style="solid" size="small">
          <RadioButton v-for="r in RANGES" :key="r.hours" :value="r.hours">{{ r.label }}</RadioButton>
        </RadioGroup>
      </template>
      <Spin :spinning="historyLoading">
        <Alert v-if="historyError" class="mb-2" :message="historyError.text" show-icon type="error">
          <template #description><ErrorFeedback :feedback="historyError" hide-message /></template>
        </Alert>
        <div v-if="history" class="text-muted-foreground mb-2 flex flex-wrap gap-x-4 text-xs">
          <span>粒度：{{ RESOLUTION_NAMES[history.resolution] ?? history.resolution }}</span>
          <span>覆盖率：{{ coverageText }}</span>
          <span v-if="gapAreas.length > 0">橙色区域为缺测时段</span>
          <span v-if="gapReasons.length > 0">缺测原因：{{ gapReasons.map((r) => QUALITY_REASONS[r] ?? r).join('、') }}</span>
        </div>
        <Alert v-if="truncated" class="mb-2" message="数据量过大，请缩小时间范围" show-icon type="warning" />
        <Empty v-if="!historyLoading && points.length === 0" description="该时段没有数据" />
        <div v-show="points.length > 0">
          <EchartsUI ref="usageChart" height="260px" />
          <EchartsUI ref="memoryChart" class="mt-2" height="220px" />
        </div>
      </Spin>
    </Card>
  </div>
</template>
