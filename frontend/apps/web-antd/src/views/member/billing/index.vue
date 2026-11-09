<script lang="ts" setup>
import type { EChartsOption } from 'echarts';
import type { EchartsUIType } from '@vben/plugins/echarts';
import type { BillingDistribution, BillingInstanceOptions, BillingInstances, BillingOverview, BillingPeriod, BillingTrend } from '#/api/types';
import type { ErrorText } from '#/locales/errors';
import { computed, onBeforeUnmount, ref, shallowRef, watch } from 'vue';
import { Page } from '@vben/common-ui';
import { EchartsUI, useEcharts } from '@vben/plugins/echarts';
import { useAccessStore } from '@vben/stores';
import { Alert, Button, Card, Empty, InputSearch, Pagination, Select, Spin, Tooltip } from 'ant-design-vue';
import { memberApi } from '#/api/backend';
import ErrorFeedback from '#/components/error-feedback.vue';
import StatusTag from '#/components/status-tag.vue';
import TimeText from '#/components/time-text.vue';
import { usePolling } from '#/composables/api';
import { describeError } from '#/locales/errors';
import { useAuthStore } from '#/store/auth';
import { formatPt } from '#/utils/format';
import { bucketLabel, bucketTooltip, forecastText } from './display';
import DistributionPie from './distribution-pie.vue';
import BillingEvents from './billing-events.vue';
import { instanceDistribution, typeDistribution } from './distribution';

const auth = useAuthStore();
const access = useAccessStore();
const eventCard = ref<InstanceType<typeof BillingEvents>>();
const identity = computed(() => !access.loginExpired && auth.me?.user.role === 'user' ? auth.me.user.id : null);
const period = ref<BillingPeriod>('today');
const distributionPeriod = ref<BillingPeriod>('today');
const periods: { label: string; value: BillingPeriod }[] = [
  { label: '今日', value: 'today' }, { label: '本周', value: 'week' },
  { label: '本月', value: 'month' }, { label: '本年', value: 'year' },
];
const instanceId = ref<string>();
const selectedLabel = ref('');
const search = ref('');
const page = ref(1);
const selectorSearch = ref('');
const selectorPage = ref(1);
const expanded = ref<string[]>([]);
const overview = shallowRef<BillingOverview | null>(null);
const distribution = shallowRef<BillingDistribution | null>(null);
const trend = shallowRef<BillingTrend | null>(null);
const instances = shallowRef<BillingInstances | null>(null);
const choices = shallowRef<BillingInstanceOptions | null>(null);
const errors = ref<Record<string, ErrorText | null>>({});
const loading = ref<Record<string, boolean>>({});
type Resource = 'overview' | 'distribution' | 'trend' | 'instances' | 'choices';
const sequences: Record<Resource, number> = { overview: 0, distribution: 0, trend: 0, instances: 0, choices: 0 };
let generation = 0;
let disposed = false;
let searchTimer: ReturnType<typeof setTimeout> | undefined;
const anyLoading = computed(() => !!eventCard.value?.loading || Object.values(loading.value).some(Boolean));
const partial = computed(() => overview.value?.partial || trend.value?.partial || instances.value?.partial);
const typeSlices = computed(() => distribution.value ? typeDistribution(distribution.value) : []);
const instanceSlices = computed(() => distribution.value ? instanceDistribution(distribution.value) : []);
const options = computed(() => {
  const result = (choices.value?.items ?? []).map((item) => ({ value: item.instance_id, label: `${item.name} #${item.instance_id}` }));
  if (instanceId.value && !result.some((item) => item.value === instanceId.value)) result.unshift({ value: instanceId.value, label: selectedLabel.value });
  return result;
});
async function load(resource: Resource, clear = false) {
  const seq = ++sequences[resource];
  const user = identity.value;
  const token = generation;
  if (clear) {
    if (resource === 'distribution') distribution.value = null;
    if (resource === 'trend') trend.value = null;
    if (resource === 'instances') instances.value = null;
    if (resource === 'choices') choices.value = null;
  }
  errors.value[resource] = null;
  loading.value[resource] = false;
  if (!user || disposed) return;
  const current = () => !disposed && token === generation && seq === sequences[resource] && user === identity.value;
  loading.value[resource] = true;
  try {
    if (resource === 'overview') {
      const result = await memberApi.billingOverview();
      if (current()) overview.value = result.data;
    } else if (resource === 'distribution') {
      const result = await memberApi.billingDistribution({ period: distributionPeriod.value });
      if (current()) distribution.value = result.data;
    } else if (resource === 'trend') {
      const result = await memberApi.billingTrend({ period: period.value, instance_id: instanceId.value });
      if (current()) trend.value = result.data;
    } else if (resource === 'choices') {
      const result = await memberApi.billingInstanceOptions({ search: selectorSearch.value, page: selectorPage.value, page_size: 20 });
      if (current()) choices.value = result.data;
    } else {
      const result = await memberApi.billingInstances({ period: period.value, instance_id: instanceId.value, search: search.value, page: page.value, page_size: 20 });
      if (current()) instances.value = result.data;
    }
  } catch (error) {
    if (current()) errors.value[resource] = describeError(error);
  } finally {
    if (current()) loading.value[resource] = false;
  }
}
function refresh(includeEvents = true) {
  void load('overview'); void load('distribution'); void load('trend'); void load('instances');
  if (includeEvents) eventCard.value?.refresh();
}
function choose(value: unknown) {
  const id = typeof value === 'string' ? value : undefined;
  selectedLabel.value = options.value.find((item) => item.value === id)?.label ?? '';
  instanceId.value = id;
}
function openChoices(open: boolean) {
  if (open && !choices.value && !loading.value.choices) void load('choices');
}
function searchChoices(value: string) {
  clearTimeout(searchTimer);
  searchTimer = setTimeout(() => { selectorSearch.value = value; selectorPage.value = 1; void load('choices', true); }, 250);
}
function searchInstances(value: string) {
  search.value = value; page.value = 1; void load('instances', true);
}
function toggle(id: string) {
  expanded.value = expanded.value.includes(id) ? expanded.value.filter((item) => item !== id) : [...expanded.value, id];
}
const chart = ref<EchartsUIType>();
const { renderEcharts } = useEcharts(chart);
watch([trend, chart], () => {
  const data = trend.value;
  if (!data || !chart.value || !identity.value) return;
  const option: EChartsOption = {
    animation: false,
    grid: { top: 20, left: 12, right: 12, bottom: 28, containLabel: true },
    tooltip: { trigger: 'axis', renderMode: 'richText', formatter: (params: unknown) => {
      const items = params as { dataIndex: number }[];
      return bucketTooltip(data, items[0]?.dataIndex ?? -1);
    } },
    xAxis: { type: 'category', data: data.items.map((item) => bucketLabel(data, item.from)), axisLabel: { hideOverlap: true } },
    yAxis: { type: 'value', name: 'Pt', min: 0, axisLabel: { formatter: (value: number) => formatPt(String(value)) } },
    series: [{ type: 'bar', name: '消耗', barMaxWidth: 36, data: data.items.map((item) => ({
      value: item.consumed_pt === null ? null : Number(item.consumed_pt),
      itemStyle: { opacity: item.partial ? 0.5 : 1, borderRadius: [3, 3, 0, 0] },
    })) }],
  };
  void renderEcharts(option);
}, { flush: 'post' });
watch(identity, () => {
  ++generation;
  clearTimeout(searchTimer);
  overview.value = null; distribution.value = null; trend.value = null; instances.value = null; choices.value = null;
  errors.value = {}; loading.value = {}; instanceId.value = undefined; selectedLabel.value = '';
  search.value = ''; selectorSearch.value = ''; page.value = 1; selectorPage.value = 1; expanded.value = [];
  refresh(false);
}, { immediate: true, flush: 'sync' });
watch(distributionPeriod, () => { void load('distribution', true); }, { flush: 'sync' });
watch([period, instanceId], () => {
  page.value = 1; expanded.value = [];
  void load('trend', true); void load('instances', true);
}, { flush: 'sync' });
usePolling(() => { if (!anyLoading.value) refresh(); }, () => 30_000);
onBeforeUnmount(() => { disposed = true; ++generation; clearTimeout(searchTimer); });
</script>

<template>
  <Page title="账单与消耗" description="按北京时间统计使用消耗">
    <Alert v-if="!identity" message="请使用普通用户账号登录后查看账单。" type="info" show-icon />
    <div v-else class="flex min-w-0 flex-col gap-4">
      <div class="flex flex-wrap items-center justify-between gap-2">
        <p class="text-muted-foreground m-0 text-sm">全账户 · 今日 00:00 至今<template v-if="overview"> · 更新于 <TimeText :value="overview.as_of" /></template></p>
        <Button :loading="anyLoading" @click="refresh()">刷新数据</Button>
      </div>
      <Alert v-if="errors.overview" message="账户汇总更新失败，已有数据为上次结果。" type="warning" show-icon><template #description><ErrorFeedback :feedback="errors.overview" /></template></Alert>
      <Spin :spinning="!!loading.overview && !overview">
        <Card>
          <div class="grid min-w-0 grid-cols-2 gap-6 md:grid-cols-3">
            <div class="col-span-2 min-w-0 md:col-span-1">
              <Tooltip :title="overview ? `已记录 ${formatPt(overview.periods.today.recorded_pt)} Pt；未结算尾段估算 ${formatPt(overview.periods.today.estimated_pt)} Pt` : '等待服务端数据'">
                <span class="text-muted-foreground text-sm">今日总消耗 ⓘ</span>
              </Tooltip>
              <div class="mt-2 break-all font-mono text-4xl font-semibold sm:text-5xl">{{ formatPt(overview?.periods.today.consumed_pt) }} <span class="text-base font-normal">Pt</span></div>
              <div class="text-muted-foreground mt-2 break-words text-xs">可用余额 {{ formatPt(overview?.balance.available_pt) }} Pt</div>
            </div>
            <div class="min-w-0">
              <p class="text-muted-foreground m-0 text-sm">当前消耗速率</p>
              <div class="mt-2 break-all font-mono text-2xl font-medium">{{ overview?.current.status === 'ready' ? `${formatPt(overview.current.hourly_pt)} Pt/h` : '暂不可用' }}</div>
              <p class="text-muted-foreground mb-0 mt-2 text-xs">全账户当前计费工作负载</p>
            </div>
            <div class="min-w-0">
              <Tooltip title="当前可用额度 ÷ 当前消耗速率，按原始精确值静态折算；忽略额度到期及每日刷新，不计未来发放，不是实际停机期限。"><span class="text-muted-foreground text-sm">当前额度可用时长 ⓘ</span></Tooltip>
              <div class="mt-2 break-words text-2xl font-medium">{{ forecastText(overview?.forecast) }}</div>
              <p v-if="overview?.forecast.exhausts_at && (overview.forecast.status === 'estimated' || overview.forecast.status === 'exhausted')" class="text-muted-foreground mb-0 mt-2 text-xs">静态折算时刻 <TimeText :value="overview.forecast.exhausts_at" /></p>
            </div>
          </div>
        </Card>
      </Spin>
      <section aria-label="消耗分布" class="min-w-0 space-y-3">
        <div class="flex min-w-0 flex-wrap items-end justify-between gap-3">
          <div class="w-full sm:w-44"><label class="mb-1 block text-sm" for="billing-distribution-period">分布统计窗口</label><Select id="billing-distribution-period" v-model:value="distributionPeriod" :options="periods" class="w-full" /></div>
          <p v-if="distribution" class="text-muted-foreground m-0 min-w-0 break-words text-xs">全账户 · <TimeText :value="distribution.from" /> 至 <TimeText :value="distribution.to" /> · 更新于 <TimeText :value="distribution.as_of" /></p>
        </div>
        <Alert v-if="errors.distribution" :message="distribution ? '分布更新失败，已有图表为同一窗口的上次结果。' : '分布加载失败，当前窗口暂不可用。'" type="warning" show-icon><template #description><ErrorFeedback :feedback="errors.distribution" /></template></Alert>
        <div v-if="distribution" class="text-muted-foreground min-w-0 break-words text-xs" role="status">
          合计 {{ formatPt(distribution.consumed_pt) }} Pt · 已记录 {{ formatPt(distribution.recorded_pt) }} Pt · 未结算尾段估算 {{ formatPt(distribution.estimated_pt) }} Pt
          <span v-if="distribution.partial" class="text-amber-700 dark:text-amber-400"> · 分布数据不完整，部分消耗待确认；当前金额不代表未知费用为 0。</span>
        </div>
        <Spin :spinning="!!loading.distribution">
          <div class="grid min-w-0 grid-cols-1 gap-3 lg:grid-cols-2">
            <DistributionPie :key="`type-${distributionPeriod}`" title="计费类型分布" :slices="typeSlices" :available="!!distribution" :loading="!!loading.distribution" :partial="!!distribution?.partial" />
            <DistributionPie :key="`instance-${distributionPeriod}`" title="实例消耗分布" :slices="instanceSlices" :available="!!distribution" :loading="!!loading.distribution" :partial="!!distribution?.partial" />
          </div>
        </Spin>
        <p class="text-muted-foreground m-0 text-xs">包含全部运行中实例（含零消耗）及所选窗口有消耗的历史实例；与下方趋势、实例筛选独立。金额包含尾段估算，悬停查看金额与占比。</p>
      </section>
      <Alert v-if="partial" message="部分消耗或当前状态尚待确认，请稍后刷新。" type="warning" show-icon />
      <Card title="消耗趋势">
        <div class="mb-4 grid min-w-0 grid-cols-1 gap-3 sm:grid-cols-2">
          <div><label class="mb-1 block text-sm" for="billing-period">统计周期</label><Select id="billing-period" v-model:value="period" :options="periods" class="w-full" /></div>
          <div><label class="mb-1 block text-sm" for="billing-instance">实例筛选</label>
            <Select id="billing-instance" :value="instanceId" :options="options" :filter-option="false" :loading="!!loading.choices" allow-clear show-search class="w-full" placeholder="全部实例" @dropdown-visible-change="openChoices" @search="searchChoices" @change="choose">
              <template #dropdownRender="{ menuNode }"><component :is="menuNode" /><div class="border-border flex items-center justify-between gap-2 border-t p-2"><Button size="small" :disabled="selectorPage <= 1 || !!loading.choices" @mousedown.prevent @click="selectorPage--; load('choices', true)">上一页</Button><span class="text-xs">{{ selectorPage }} / {{ Math.max(1, Math.ceil((choices?.total ?? 0) / 20)) }}</span><Button size="small" :disabled="selectorPage * 20 >= (choices?.total ?? 0) || !!loading.choices" @mousedown.prevent @click="selectorPage++; load('choices', true)">下一页</Button></div></template>
            </Select>
            <div v-if="errors.choices" class="text-destructive mt-1 text-xs" role="alert">实例选项加载失败 <Button size="small" type="link" @click="load('choices')">重试</Button></div>
          </div>
        </div>
        <Alert v-if="errors.trend" message="趋势更新失败，已有图表为上次结果。" type="warning" show-icon><template #description><ErrorFeedback :feedback="errors.trend" /></template></Alert>
        <Spin :spinning="!!loading.trend"><EchartsUI v-if="trend" ref="chart" class="h-[280px] w-full" /><Empty v-else :description="loading.trend ? '正在加载趋势' : '趋势暂不可用'" /></Spin>
        <p class="text-muted-foreground mb-0 mt-2 text-xs">浅色柱为未完整时段；未来时段留空。悬停查看已记录与尾段估算。<template v-if="trend"> 更新于 <TimeText :value="trend.as_of" /></template></p>
      </Card>
      <Card title="实例消耗">
        <div class="mb-4 max-w-md"><InputSearch aria-label="搜索实例名称或 ID" placeholder="搜索实例名称或 ID" enter-button="搜索" :default-value="search" @search="searchInstances" /></div>
        <Alert v-if="errors.instances" message="实例消耗更新失败，已有列表为上次结果。" type="warning" show-icon><template #description><ErrorFeedback :feedback="errors.instances" /></template></Alert>
        <Spin :spinning="!!loading.instances">
          <div class="space-y-3">
            <article v-for="item in instances?.items ?? []" :key="item.instance_id" class="border-border min-w-0 rounded-lg border p-4">
              <div class="flex flex-wrap items-start justify-between gap-3">
                <div class="min-w-0 flex-1"><div class="flex flex-wrap items-center gap-2"><RouterLink :to="`/instances/${item.instance_id}`" class="text-primary break-all font-medium">{{ item.name }}</RouterLink><StatusTag kind="observed" :value="item.instance_state" /></div><span class="text-muted-foreground text-xs">#{{ item.instance_id }}</span></div>
                <Tooltip :title="`已记录 ${formatPt(item.recorded_pt)} Pt；未结算尾段估算 ${formatPt(item.estimated_pt)} Pt`"><span class="break-all font-mono font-semibold">{{ formatPt(item.consumed_pt) }} Pt</span></Tooltip>
              </div>
              <div class="mt-3 flex flex-wrap items-center justify-between gap-2"><span class="text-muted-foreground break-all text-sm">当前速率 {{ item.rate_status === 'ready' ? `${formatPt(item.current_hourly_pt)} Pt/h` : '暂不可用' }}</span><Button size="small" type="link" :aria-expanded="expanded.includes(item.instance_id)" @click="toggle(item.instance_id)">{{ expanded.includes(item.instance_id) ? '收起明细' : '费用明细' }}</Button></div>
              <dl v-if="expanded.includes(item.instance_id)" class="border-border mb-0 mt-3 grid grid-cols-1 gap-3 border-t pt-3 sm:grid-cols-2 lg:grid-cols-4"><div v-for="(label, key) in { cpu_pt: 'CPU', memory_pt: '内存', disk_pt: '磁盘', gpu_pt: 'GPU' }" :key="key" class="min-w-0"><dt class="text-muted-foreground text-xs">{{ label }}</dt><dd class="m-0 mt-1 break-all font-mono">{{ formatPt(item.breakdown[key]) }} Pt</dd></div></dl>
            </article>
            <Empty v-if="!instances?.items.length" :description="loading.instances ? '正在加载实例消耗' : errors.instances ? '实例消耗暂不可用' : '没有匹配的实例'" />
          </div>
        </Spin>
        <div class="mt-4 flex flex-wrap items-center justify-between gap-3"><span class="text-muted-foreground text-xs">共 {{ instances?.total ?? '—' }} 个实例<template v-if="instances"> · <TimeText :value="instances.as_of" /></template></span><Pagination :current="page" :total="instances?.total ?? 0" :page-size="20" :show-size-changer="false" size="small" @change="(value: number) => { page = value; expanded = []; load('instances', true); }" /></div>
      </Card>
      <BillingEvents ref="eventCard" :identity="identity" />
    </div>
  </Page>
</template>
