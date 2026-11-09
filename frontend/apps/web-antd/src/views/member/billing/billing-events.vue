<script lang="ts" setup>
import type { BillingEvent, BillingEvents, BillingPeriod } from '#/api/types';
import type { ErrorText } from '#/locales/errors';
import { onBeforeUnmount, ref, shallowRef, watch } from 'vue';
import { Alert, Button, Card, Empty, Pagination, Select, Spin } from 'ant-design-vue';
import { memberApi } from '#/api/backend';
import ErrorFeedback from '#/components/error-feedback.vue';
import TimeText from '#/components/time-text.vue';
import { describeError } from '#/locales/errors';
import { formatPt } from '#/utils/format';

const props = defineProps<{ identity: string | null }>();
const period = ref<BillingPeriod>('today');
const page = ref(1);
const data = shallowRef<BillingEvents | null>(null);
const error = shallowRef<ErrorText | null>(null);
const loading = ref(false);
const periods = [
  { label: '今日', value: 'today' }, { label: '本周', value: 'week' },
  { label: '本月', value: 'month' }, { label: '本年', value: 'year' },
];
const names: Record<BillingEvent['event_type'], string> = {
  'credit.grant': '额度发放', 'instance.create': '实例新建',
  'instance.start': '实例启动', 'instance.stop': '实例停止', 'instance.delete': '实例删除',
};
let sequence = 0;
let disposed = false;
function amount(value: string | null) { return value === null ? '未知' : formatPt(value); }
const grantSources: Record<string, string> = { daily: '每日额度（含注册即时发放）', redemption: '兑换码', admin: '管理员发放' };
function source(value: string | null) { return value === null ? '未知' : grantSources[value] ?? value; }
async function load(clear = false) {
  const seq = ++sequence;
  const user = props.identity;
  if (clear) data.value = null;
  error.value = null;
  loading.value = false;
  if (!user || disposed) return;
  const current = () => !disposed && seq === sequence && user === props.identity;
  loading.value = true;
  try {
    const result = await memberApi.billingEvents({ period: period.value, page: page.value, page_size: 20 });
    if (current()) data.value = result.data;
  } catch (cause) {
    if (current()) error.value = describeError(cause);
  } finally {
    if (current()) loading.value = false;
  }
}
function refresh() { if (!loading.value) void load(); }
function changePage(value: number) { page.value = value; void load(true); }
watch(period, () => { page.value = 1; void load(true); }, { flush: 'sync' });
watch(() => props.identity, () => { page.value = 1; void load(true); }, { immediate: true, flush: 'sync' });
onBeforeUnmount(() => { disposed = true; ++sequence; });
defineExpose({ refresh, loading });
</script>

<template>
  <Card title="额度事件" class="min-w-0">
    <div class="mb-3 flex min-w-0 flex-wrap items-end justify-between gap-3">
      <div class="w-full sm:w-44"><label for="billing-events-period" class="mb-1 block text-sm">事件时间窗口</label><Select id="billing-events-period" v-model:value="period" :options="periods" class="w-full" /></div>
      <p class="text-muted-foreground m-0 min-w-0 break-words text-xs">仅展示功能启用后记录的成功事件；时间为北京时间。与消耗统计筛选独立。</p>
    </div>
    <Alert v-if="error" :message="data ? '额度事件更新失败，保留同一筛选的上次结果。' : '额度事件加载失败。'" type="warning" show-icon class="mb-3"><template #description><ErrorFeedback :feedback="error" /><Button size="small" type="link" @click="refresh">重试</Button></template></Alert>
    <Spin :spinning="loading">
      <ol v-if="data?.items.length" class="m-0 list-none space-y-3 p-0" aria-label="额度事件记录">
        <li v-for="item in data.items" :key="item.id" class="border-border min-w-0 border-b pb-3 last:border-b-0">
          <div class="flex min-w-0 flex-wrap items-center justify-between gap-2"><span class="font-medium">{{ names[item.event_type] }}</span><span class="text-muted-foreground text-xs"><TimeText :value="item.occurred_at" /></span></div>
          <p v-if="item.event_type !== 'credit.grant' || item.instance_id !== null" class="text-muted-foreground mb-1 mt-1 break-all text-xs">{{ item.instance_name ?? '未知实例' }} · #{{ item.instance_id ?? '未知' }}</p>
          <p v-if="item.event_type === 'credit.grant'" class="mb-1 mt-1 break-all text-sm">发放来源：{{ source(item.grant_source) }} · 发放额 <span class="font-mono">{{ amount(item.amount_pt) }} Pt</span></p>
          <div class="mt-1 flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1 text-sm">
            <span>{{ item.event_type === 'credit.grant' ? '可用额度' : '全账户总消耗速率' }}</span>
            <span class="break-all font-mono">前 {{ amount(item.event_type === 'credit.grant' ? item.available_before_pt : item.hourly_before_pt) }} {{ item.event_type === 'credit.grant' ? 'Pt' : 'Pt/h' }}</span>
            <span aria-label="变为">→</span>
            <span class="break-all font-mono">后 {{ amount(item.event_type === 'credit.grant' ? item.available_after_pt : item.hourly_after_pt) }} {{ item.event_type === 'credit.grant' ? 'Pt' : 'Pt/h' }}</span>
          </div>
        </li>
      </ol>
      <Empty v-else :description="loading ? '正在加载额度事件' : error ? '额度事件暂不可用' : '当前窗口没有已记录事件'" />
    </Spin>
    <div class="mt-3 flex min-w-0 flex-wrap items-center justify-between gap-3"><span class="text-muted-foreground min-w-0 break-words text-xs">共 {{ data?.total ?? '—' }} 条<template v-if="data"> · 更新于 <TimeText :value="data.as_of" /></template></span><Pagination :current="page" :total="data?.total ?? 0" :page-size="20" :show-size-changer="false" size="small" @change="changePage" /></div>
  </Card>
</template>
