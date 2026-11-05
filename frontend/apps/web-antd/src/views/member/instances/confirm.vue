<script lang="ts" setup>
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue';
import { useRouter } from 'vue-router';

import { Page } from '@vben/common-ui';
import { useAccessStore } from '@vben/stores';

import { Alert, Button, Card, Empty, Tag } from 'ant-design-vue';

import ErrorFeedback from '#/components/error-feedback.vue';
import OsIcon from '#/components/os-icon.vue';
import { usePolling } from '#/composables/api';
import { useAuthStore } from '#/store';
import { useInstanceOrderStore } from '#/store/instance-order';
import { formatEstimatedRuntime } from '#/utils/creation-math';
import { formatBytes, formatPt, formatTime } from '#/utils/format';
import { quoteFitsAvailableCredit } from '#/utils/order-terms';

const router = useRouter();
const auth = useAuthStore();
const access = useAccessStore();
const store = useInstanceOrderStore();
const identity = computed(() => auth.me?.user.role === 'user' && !access.loginExpired ? auth.me.user.id : null);
const order = computed(() => store.order);
const quote = computed(() => store.quote);
const now = ref(Date.now());
let disposed = false;
let session = 0;
let clock: number | undefined;

const quoteExpired = computed(() => !!quote.value && !(Date.parse(quote.value.expires_at) > now.value));
const quoteRemaining = computed(() => {
  if (!quote.value || quoteExpired.value) return '';
  const seconds = Math.max(0, Math.ceil((Date.parse(quote.value.expires_at) - now.value) / 1000));
  return `${Math.floor(seconds / 60)} 分 ${String(seconds % 60).padStart(2, '0')} 秒`;
});
const usableQuote = computed(() => quote.value && !quoteExpired.value && quote.value.admissible && !store.quoteError && !store.quoteLoading && !store.needsEdit ? quote.value : null);
const accountBlock = computed(() => {
  if (store.accountError) return '额度读取失败，暂不能估算时长或创建，请重试。';
  if (store.accountLoading) return '正在核对最新可用额度，暂不能创建。';
  if (!store.account) return '尚未获取可用额度，请刷新额度后再确认。';
  if (store.account.status !== 'active') return '额度账户当前不可用，暂不能估算时长或创建。';
  if (store.account.enforcements.some((e) => e.status !== 'cancelled')) return '账户存在额度停机事件，请先核对额度与停机状态；暂不能估算正常可用时长或创建。';
  if (quote.value && !quoteFitsAvailableCredit(quote.value, store.account.available_pt)) return '可用额度不足以支付首个计费窗口，或当前报价缺少有效预留金额，请更新报价。';
  return '';
});
const runtime = computed(() => usableQuote.value && store.account && !accountBlock.value
  ? formatEstimatedRuntime(store.account.available_pt, usableQuote.value.price_details.total_hourly_pt)
  : '暂不可估算');
const canRefreshQuote = computed(() => !!order.value && !!identity.value && !store.locked && !store.quoteLoading && !store.needsEdit);
const primaryDisabled = computed(() => {
  if (!identity.value || !order.value || store.submitting) return true;
  if (store.hasPendingIntent) return false;
  if (store.needsEdit) return store.locked;
  if (quoteExpired.value || !quote.value || store.quoteError) return !canRefreshQuote.value;
  return !usableQuote.value || !!accountBlock.value || store.locked;
});
const primaryLabel = computed(() => {
  if (store.submitting) return '正在提交创建';
  if (store.hasPendingIntent) return '使用同一请求重试';
  if (store.needsEdit) return '返回修改配置';
  if (quoteExpired.value) return '报价已过期，更新报价';
  if (!quote.value || store.quoteError) return '更新报价';
  if (store.quoteLoading) return '正在更新报价';
  return '确认并创建实例';
});
const priceRows = computed(() => [
  { label: 'CPU', value: quote.value?.price_details.cpu_hourly_pt },
  { label: '内存', value: quote.value?.price_details.memory_hourly_pt },
  { label: '磁盘', value: quote.value?.price_details.disk_hourly_pt },
  { label: 'GPU', value: quote.value?.price_details.gpu_hourly_pt },
]);
const networkMode = computed(() => order.value ? {
  bridge: '桥接网络',
  macvlan: 'Macvlan 直连',
  managed_bridge: '托管桥接网络',
}[order.value.network.mode] : '');
const REASONS: Record<string, string> = {
  credit_account_unavailable: '额度账户不可用',
  insufficient_credit: '可用额度不足以预留首个计费窗口',
  gpu_price_exceeds_limit: 'GPU 当前单价高于所设上限，请返回配置调整价格上限，或选择不带 GPU',
  insufficient_disk_bytes: '节点可用磁盘不足',
  insufficient_gpu_capacity_or_metrics: '符合价格上限的 GPU 库存不足或监控数据不可用',
  insufficient_memory_bytes: '节点可用内存不足',
  insufficient_vcpus: '节点可用 CPU 不足',
  inventory_stale: '节点库存观测已过期，请等待刷新',
  no_registered_address_available: '网络暂无可分配的已登记地址',
  node_unavailable: '节点暂不可用',
  quota_exceeded_disk_bytes: '超出账户磁盘配额',
  quota_exceeded_gpus: '超出账户 GPU 配额',
  quota_exceeded_instances: '超出账户实例数量配额',
  quota_exceeded_memory_bytes: '超出账户内存配额',
  quota_exceeded_vcpus: '超出账户 CPU 配额',
  quota_not_configured: '账户配额尚未配置，请联系管理员',
};
const blockingReasons = computed(() => quote.value?.blocking_reasons.map((reason) => REASONS[reason] ?? `暂不满足创建条件（${reason}），请联系管理员`) ?? []);

function editConfiguration() {
  if (!store.locked) void router.push('/instances/create');
}

async function refreshAccount() {
  if (order.value && identity.value && !store.locked && !store.accountLoading && !store.needsEdit) await store.refreshAccount();
}

async function confirmOrder() {
  if (primaryDisabled.value) return;
  if (store.needsEdit && !store.hasPendingIntent) {
    editConfiguration();
    return;
  }
  // 原请求结果不确定时，重试必须交给 store 复用同一请求，不能先刷新报价。
  if (!store.hasPendingIntent && (quoteExpired.value || !quote.value || store.quoteError)) {
    await store.refreshQuote();
    return;
  }
  const token = session;
  const userId = identity.value;
  const accepted = await store.submit();
  if (accepted && !disposed && session === token && userId !== null && identity.value === userId) {
    await router.replace({ name: 'MemberInstanceInitialization', params: { taskId: accepted.task_id } });
  }
}

watch(identity, () => { ++session; }, { flush: 'sync' });
usePolling(refreshAccount, () => 30_000);
onMounted(() => {
  now.value = Date.now();
  clock = window.setInterval(() => { now.value = Date.now(); }, 1000);
  void refreshAccount();
});
onBeforeUnmount(() => {
  disposed = true;
  ++session;
  window.clearInterval(clock);
});
</script>

<template>
  <Page title="确认订单" description="核对配置与当前小时报价。仅在最终确认后提交创建，不涉及订单支付。">
    <Card v-if="!order" class="empty-order">
      <Empty description="暂无待确认的配置，或本次配置已失效">
        <p class="text-muted-foreground text-sm">配置仅在当前会话内保留，刷新或直接访问此页面后，请重新选择配置。</p>
        <Button type="primary" :disabled="store.locked" @click="editConfiguration">返回配置页</Button>
      </Empty>
    </Card>
    <template v-else>
      <div class="mb-4 flex flex-wrap items-center justify-between gap-3">
        <Button :disabled="store.locked" @click="editConfiguration">返回修改配置</Button>
        <Tag color="blue" class="m-0">第二步 · 确认后创建</Tag>
      </div>
      <Alert v-if="store.notice" class="mb-4" :message="store.notice" :description="store.needsEdit ? '当前配置不能继续创建，请返回修改并获取最新报价。' : '请核对最新费用与预留额度后再次确认，本次更新不会自动提交创建。'" show-icon type="warning" role="alert" />
      <Alert v-if="store.hasPendingIntent && !store.submitting" class="mb-4" message="创建结果尚未确定，请勿重复创建" description="配置与登录凭据已锁定。请使用最终按钮，以相同幂等键和相同请求重试；不要刷新页面或另开页面再次创建。" show-icon type="warning" />
      <Alert v-if="store.submitError" class="mb-4" message="创建请求未完成" show-icon type="error">
        <template #description><ErrorFeedback :feedback="store.submitError" /></template>
      </Alert>

      <div class="confirmation-grid">
        <div class="confirmation-main min-w-0">
          <Card title="配置清单" size="small">
            <dl class="configuration-list m-0">
              <div><dt>实例名称</dt><dd class="font-semibold">{{ order.spec.name }}</dd></div>
              <div><dt>套餐</dt><dd>{{ order.plan.name }}</dd></div>
              <div><dt>系统镜像</dt><dd class="flex items-start gap-3"><OsIcon :name="order.image.os_name" class="shrink-0 text-[28px]" /><span><strong class="block">{{ order.image.os_name || 'Linux' }} {{ order.image.os_release }}</strong><span class="text-muted-foreground block text-xs">{{ order.image.name }} · {{ order.image.architecture }}</span></span></dd></div>
              <div><dt>登录用户</dt><dd>root</dd></div>
              <div v-if="store.loginSummary?.mode === 'password'"><dt>root 密码</dt><dd data-testid="root-password-summary">••••••••（已设置）</dd></div>
              <div v-else-if="store.loginSummary?.mode === 'ssh-key'"><dt>SSH 公钥</dt><dd data-testid="ssh-public-key-summary">{{ store.loginSummary.sshKeyCount }} 条</dd></div>
              <div><dt>CPU</dt><dd>{{ order.spec.resources.vcpus }} vCPU</dd></div>
              <div><dt>内存</dt><dd>{{ formatBytes(order.spec.resources.memory_bytes) }}</dd></div>
              <div><dt>磁盘</dt><dd>{{ formatBytes(order.spec.resources.disk_bytes) }}</dd></div>
              <div><dt>GPU</dt><dd>{{ order.spec.gpu.count }} 张<span v-if="order.spec.gpu.count"> · {{ order.spec.gpu.model }} · {{ order.spec.gpu.access_mode === 'shared' ? '共享' : '独占' }}</span><span v-else> · 不使用 GPU</span></dd></div>
              <div v-if="order.spec.gpu.count"><dt>GPU 价格上限</dt><dd>{{ formatPt(order.spec.gpu.max_hourly_pt) }} Pt/小时<span class="text-muted-foreground mt-1 block text-xs">价格上限不是实际报价</span></dd></div>
              <div><dt>网络</dt><dd>{{ networkMode }}</dd></div>
              <div><dt>节点</dt><dd class="font-mono">{{ order.network.node_id }}</dd></div>
              <div><dt>项目</dt><dd>{{ order.network.project_name }}</dd></div>
              <div><dt>IP 地址</dt><dd>创建时自动分配<span class="text-muted-foreground mt-1 block text-xs">以实际创建结果为准，不预留或预先指定地址</span></dd></div>
            </dl>
            <p class="text-muted-foreground mb-0 mt-4 text-xs">实例名称仅用于识别，不影响报价或预计可用时长。</p>
          </Card>

          <Card title="小时费用明细" size="small">
            <dl class="m-0 space-y-3">
              <div v-for="row in priceRows" :key="row.label" class="price-row"><dt class="text-muted-foreground">{{ row.label }}</dt><dd>{{ formatPt(row.value) }} Pt/小时</dd></div>
            </dl>
            <p class="text-muted-foreground mb-0 mt-4 text-xs">按服务端当前报价展示，以实际资源使用和计费政策结算。</p>
          </Card>
        </div>

        <div class="confirmation-sidebar min-w-0">
          <Card title="确认费用与额度" size="small" class="summary-card">
            <template #extra><Button size="small" :disabled="!canRefreshQuote" :loading="store.quoteLoading" @click="store.refreshQuote">更新报价</Button></template>
            <div class="space-y-4" :aria-busy="store.quoteLoading">
              <div>
                <p class="text-muted-foreground mb-1 text-sm">当前合计</p>
                <p class="mb-0"><strong class="text-primary break-all text-3xl tabular-nums">{{ formatPt(quote?.price_details.total_hourly_pt) }}</strong><span class="text-muted-foreground ml-2 text-sm">Pt/小时</span></p>
              </div>
              <p v-if="store.quoteLoading" class="text-muted-foreground mb-0 text-sm" role="status">正在更新报价，请等待后重新确认。</p>
              <ErrorFeedback v-if="store.quoteError" :feedback="store.quoteError" />
              <div v-if="quote" class="border-border border-y py-3 text-sm">
                <p class="mb-1 font-medium" :class="{ 'text-destructive': quoteExpired }">{{ quoteExpired ? '报价已过期' : `报价剩余 ${quoteRemaining}` }}</p>
                <p class="text-muted-foreground mb-0 text-xs">有效至 {{ formatTime(quote.expires_at) }}</p>
              </div>
              <Alert v-if="quoteExpired && !store.hasPendingIntent" message="旧报价不能提交创建" description="请点击“更新报价”，核对最新报价后再次确认。更新报价不会创建实例。" show-icon type="warning" />
              <Alert v-if="blockingReasons.length || (quote && !quote.admissible)" message="当前配置暂不能创建" show-icon type="warning">
                <template #description><ul class="mb-0 pl-4"><li v-for="reason in blockingReasons" :key="reason">{{ reason }}</li><li v-if="!blockingReasons.length">服务端暂不允许创建，请更新报价或联系管理员。</li></ul></template>
              </Alert>
              <dl class="m-0 space-y-3">
                <div class="price-row"><dt class="text-muted-foreground">当前可用额度</dt><dd>{{ formatPt(store.account?.available_pt) }} Pt</dd></div>
                <div class="price-row"><dt class="text-muted-foreground">首窗口预留</dt><dd>{{ formatPt(quote?.initial_window_pt) }} Pt</dd></div>
              </dl>
              <p class="text-muted-foreground mb-0 text-xs">首窗口预留来自服务端报价；可用额度已扣除现有预留及欠款，估时不再重复扣除。</p>
              <div class="border-border border-t pt-4">
                <div class="mb-2 flex items-center justify-between gap-3"><span class="font-medium">预计可用时长</span><Button size="small" :disabled="store.locked || store.needsEdit" :loading="store.accountLoading" @click="refreshAccount">刷新额度</Button></div>
                <p class="mb-2 text-xl font-semibold">{{ runtime }}</p>
                <ErrorFeedback v-if="store.accountError" :feedback="store.accountError" />
                <Alert v-else-if="accountBlock" :message="accountBlock" show-icon type="warning" />
                <p class="text-muted-foreground mb-0 mt-2 text-xs leading-relaxed">前端仅按此实例的当前报价与可用余额静态估算；不包含其他实例消耗、未来额度发放或额度到期，不保证连续运行。零费率不代表无限时长。</p>
              </div>
              <!-- <Alert message="报价不锁定资源，也不是创建承诺" description="最终确认后才提交创建；服务端会再次校验库存、额度与政策，受理后由任务中心跟踪实际创建结果。" show-icon type="info" /> -->
              <div class="desktop-submit"><Button type="primary" block size="large" :disabled="primaryDisabled" :loading="store.submitting || store.quoteLoading" @click="confirmOrder">{{ primaryLabel }}</Button></div>
            </div>
          </Card>
        </div>
      </div>
      <div class="mobile-submit">
        <div class="min-w-0 flex-1"><p class="m-0 break-all font-semibold tabular-nums">{{ formatPt(quote?.price_details.total_hourly_pt) }} <span class="text-muted-foreground text-xs font-normal">Pt/小时</span></p><p class="text-muted-foreground m-0 text-xs">预计 {{ runtime }}</p></div>
        <Button type="primary" :disabled="primaryDisabled" :loading="store.submitting || store.quoteLoading" @click="confirmOrder">{{ primaryLabel }}</Button>
      </div>
    </template>
  </Page>
</template>

<style scoped>
.confirmation-grid { display: grid; grid-template-columns: minmax(0, 1fr); gap: 1rem; padding-bottom: calc(7rem + env(safe-area-inset-bottom)); }
.confirmation-main { display: flex; flex-direction: column; gap: 1rem; }
.configuration-list > div { display: grid; grid-template-columns: 6.5rem minmax(0, 1fr); gap: 0.75rem; padding: 0.75rem 0; border-bottom: 1px solid hsl(var(--border)); }
.configuration-list > div:first-child { padding-top: 0; }
.configuration-list > div:last-child { padding-bottom: 0; border-bottom: 0; }
.configuration-list dt { color: hsl(var(--muted-foreground)); }
.configuration-list dd { min-width: 0; margin: 0; overflow-wrap: anywhere; }
.price-row { display: flex; align-items: baseline; justify-content: space-between; gap: 1rem; }
.price-row dd { margin: 0; text-align: right; overflow-wrap: anywhere; font-variant-numeric: tabular-nums; }
.mobile-submit { position: fixed; right: 0; bottom: 0; left: 0; z-index: 30; display: flex; align-items: center; gap: 0.75rem; padding: 0.75rem max(1rem, env(safe-area-inset-right)) max(0.75rem, env(safe-area-inset-bottom)) max(1rem, env(safe-area-inset-left)); border-top: 1px solid hsl(var(--border)); background: hsl(var(--background)); }
.desktop-submit { display: none; }
@media (min-width: 1024px) {
  .confirmation-grid { grid-template-columns: minmax(0, 1.2fr) minmax(22rem, 0.8fr); align-items: start; padding-bottom: 1rem; }
  .confirmation-sidebar { align-self: stretch; }
  .summary-card { position: sticky; top: 1rem; }
  .desktop-submit { display: block; }
  .mobile-submit { display: none; }
}
@media (max-width: 374px) {
  .configuration-list > div { grid-template-columns: 5rem minmax(0, 1fr); }
  .mobile-submit { flex-wrap: wrap; }
  .mobile-submit > .ant-btn { width: 100%; }
  .confirmation-grid { padding-bottom: calc(10rem + env(safe-area-inset-bottom)); }
}
</style>
