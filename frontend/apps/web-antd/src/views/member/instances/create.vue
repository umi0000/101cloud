<script lang="ts" setup>
import type { Query } from '#/api/backend';
import type { ApiResult } from '#/api/request';
import type { CreditAccount, ElasticQuote, ElasticQuoteSpec, Image, NetworkOption, Page as CursorPage, Plan, Resources } from '#/api/types';
import type { ErrorText } from '#/locales/errors';
import type { CreationFormDraft } from '#/store/instance-order';
import type { LoginDraft } from '#/utils/instance-login';

import { computed, onBeforeUnmount, ref, shallowRef, watch } from 'vue';
import { useRouter } from 'vue-router';

import { Page } from '@vben/common-ui';
import { useAccessStore } from '@vben/stores';

import { Alert, Button, Card, Empty, Input, InputNumber, InputPassword, RadioButton, RadioGroup, Select, Skeleton, Tag, Textarea } from 'ant-design-vue';

import { listAll, memberApi } from '#/api/backend';
import { ApiError, newIdempotencyKey } from '#/api/request';
import ErrorFeedback from '#/components/error-feedback.vue';
import { usePolling } from '#/composables/api';
import { describeError } from '#/locales/errors';
import { useAuthStore } from '#/store';
import { useInstanceLoginDraft, useInstanceOrderStore } from '#/store/instance-order';
import { creationResourceError, formatEstimatedRuntime } from '#/utils/creation-math';
import { formatBytes, formatPt, formatTime, ptToMicros } from '#/utils/format';
import { emptyLoginDraft, imageSupportsLogin, validateLoginDraft } from '#/utils/instance-login';

import ResourceConfigurator from './components/resource-configurator.vue';
import ImageSelector from './components/image-selector.vue';

const router = useRouter();
const auth = useAuthStore();
const access = useAccessStore();
const orders = useInstanceOrderStore();
const loginMemory = useInstanceLoginDraft();
// Credentials stay separate from resource/name form snapshots and URL state.
const loginDraft = ref<LoginDraft>(loginMemory.read());
const loginError = ref('请设置 root 密码。');
const loginValidating = ref(false);
const preparingOrder = ref(false);
let loginSequence = 0;
const identity = computed(() => auth.me?.user.role === 'user' && !access.loginExpired ? auth.me.user.id : null);
const plans = shallowRef<Plan[]>([]);
const networks = shallowRef<NetworkOption[]>([]);
const images = shallowRef<Image[]>([]);
const planId = ref<string>();
const networkId = ref<string>();
const imageId = ref<string>();
const name = ref('');
const resources = ref<Resources>({ disk_bytes: '0', memory_bytes: '0', vcpus: 0 });
const resourcesValid = ref(true);
const gpuCount = ref<number | undefined>(0);
const gpuCap = ref('0');
const plansLoading = ref(false);
const networksLoading = ref(false);
const imagesLoading = ref(false);
const accountLoading = ref(false);
const plansError = shallowRef<ErrorText | null>(null);
const networksError = shallowRef<ErrorText | null>(null);
const imagesError = shallowRef<ErrorText | null>(null);
const accountError = shallowRef<ErrorText | null>(null);
const quoteError = shallowRef<ErrorText | null>(null);
const changedError = shallowRef<ErrorText | null>(null);
const account = shallowRef<CreditAccount | null>(null);
const quote = shallowRef<{ fingerprint: string; value: ElasticQuote } | null>(null);
const quoteLoading = ref(false);
const confirmationNotice = ref('');
const now = ref(Date.now());
const locked = computed(() => orders.locked);
let pendingRestore: CreationFormDraft | null = null;
let preferredImageId: string | undefined;
let disposed = false;
let session = 0;
let plansSequence = 0;
let networksSequence = 0;
let imagesSequence = 0;
let quoteSequence = 0;
let quoteTimer: number | undefined;
const clock = window.setInterval(() => { now.value = Date.now(); }, 1000);
const isCurrent = (token: number) => !disposed && token === session && identity.value !== null;

const selectedPlan = computed(() => plans.value.find((item) => item.id === planId.value) ?? null);
const availableNetworks = computed(() => {
  const plan = selectedPlan.value;
  return plan ? networks.value.filter((item) => plan.network_option_ids.includes(item.id) && plan.node_ids.includes(item.node_id)) : [];
});
const selectedNetwork = computed(() => availableNetworks.value.find((item) => item.id === networkId.value) ?? null);
const availableImages = computed(() => {
  const plan = selectedPlan.value;
  return plan ? images.value.filter((item) => item.status === 'active' && item.instance_type === 'container' && plan.image_ids.includes(item.id)) : [];
});
const selectedImage = computed(() => availableImages.value.find((item) => item.id === imageId.value && (!gpuCount.value || item.gpu_ready)) ?? null);
const loginImageError = computed(() => selectedImage.value && selectedNetwork.value && !imageSupportsLogin(selectedImage.value, selectedNetwork.value)
  ? '所选镜像不支持 cloud-init，请更换镜像。' : '');
const nameError = computed(() => {
  if (!name.value.trim()) return '请填写实例名称';
  return Array.from(name.value.trim()).length > 128 ? '实例名称不能超过 128 个字符' : '';
});
const resourceError = computed(() => selectedPlan.value ? creationResourceError(selectedPlan.value, resources.value) : null);
const gpuError = computed(() => {
  const plan = selectedPlan.value;
  if (!plan) return '';
  const count = gpuCount.value;
  if (count === undefined || !Number.isSafeInteger(count) || count < 0) return 'GPU 数量必须是非负整数';
  const cap = ptToMicros(gpuCap.value);
  const maximum = ptToMicros(plan.gpu.max_hourly_pt);
  if (cap === null || cap < 0n || maximum === null || cap > maximum) return 'GPU 每小时价格上限必须是非负金额，且不能超过套餐上限（最多 6 位小数）';
  if (count === 0) return '';
  if (!plan.gpu.model) return '此套餐不提供 GPU';
  if (count === plan.gpu.count) return '';
  const bound = plan.allowed_overrides.gpu_count;
  if (!bound || !/^\d+$/.test(bound.min) || !/^\d+$/.test(bound.max) || !/^\d+$/.test(bound.step)) return 'GPU 数量不在套餐允许范围内';
  const min = BigInt(bound.min);
  const max = BigInt(bound.max);
  const step = BigInt(bound.step);
  const value = BigInt(count);
  return step > 0n && value >= min && value <= max && (value - min) % step === 0n ? '' : 'GPU 数量不在套餐允许的范围或步长内';
});
const gpuMaximum = computed(() => {
  const plan = selectedPlan.value;
  if (!plan?.gpu.model) return 0;
  const maximum = Number(plan.allowed_overrides.gpu_count?.max ?? plan.gpu.count);
  return Number.isSafeInteger(maximum) ? Math.max(maximum, plan.gpu.count) : plan.gpu.count;
});
const gpuRangeText = computed(() => {
  const plan = selectedPlan.value;
  if (!plan?.gpu.model) return '此套餐不提供 GPU';
  const bound = plan.allowed_overrides.gpu_count;
  return bound ? `可选 0 或 ${bound.min}–${bound.max} 张，步长 ${bound.step}；套餐基准 ${plan.gpu.count} 张也可选` : `可选 0 或 ${plan.gpu.count} 张`;
});
const accountBlock = computed(() => {
  if (accountError.value) return '额度读取失败，暂不能估算时长或创建，请重试';
  if (!account.value) return '正在获取可用额度';
  if (account.value.status !== 'active') return '额度账户当前不可用，暂不能估算时长或创建';
  if (account.value.enforcements.some((e) => e.status !== 'cancelled')) return '账户存在额度停机事件，请先核对额度与停机状态；暂不展示正常可用时长';
  return '';
});
const spec = computed<ElasticQuoteSpec | null>(() => {
  const plan = selectedPlan.value;
  const network = selectedNetwork.value;
  if (!identity.value || !plan || !network || !selectedImage.value || gpuError.value || resourceError.value || !resourcesValid.value
    || plansLoading.value || networksLoading.value || imagesLoading.value || plansError.value || networksError.value || imagesError.value) return null;
  return {
    gpu: { access_mode: plan.gpu.access_mode, count: gpuCount.value ?? 0, max_hourly_pt: gpuCap.value, model: gpuCount.value ? plan.gpu.model : null },
    image_id: selectedImage.value.id,
    init_parameters: {},
    network_option_id: network.id,
    plan_id: plan.id,
    plan_revision: plan.revision,
    resources: { ...resources.value },
  };
});
const fingerprint = computed(() => spec.value ? JSON.stringify(spec.value) : '');
const matchedQuote = computed(() => quote.value?.fingerprint === fingerprint.value && spec.value ? quote.value.value : null);
const quoteExpired = computed(() => !!matchedQuote.value && !(Date.parse(matchedQuote.value.expires_at) > now.value));
const usableQuote = computed(() => matchedQuote.value && !quoteExpired.value && matchedQuote.value.admissible ? matchedQuote.value : null);
const runtime = computed(() => !accountBlock.value && account.value && usableQuote.value ? formatEstimatedRuntime(account.value.available_pt, usableQuote.value.price_details.total_hourly_pt) : '暂不可估算');
const canCreate = computed(() => !!usableQuote.value && !nameError.value && !loginError.value && !loginImageError.value && !loginValidating.value && !preparingOrder.value && !accountBlock.value && !quoteLoading.value && !locked.value);
const canRefreshQuote = computed(() => !!spec.value && !quoteLoading.value && !locked.value);
const primaryDisabled = computed(() => preparingOrder.value || locked.value || (quoteExpired.value ? !canRefreshQuote.value : !canCreate.value));
const primaryLabel = computed(() => quoteExpired.value ? '报价已过期，重新报价' : '创建实例');
const priceRows = computed(() => {
  const price = matchedQuote.value?.price_details;
  return [
    { label: 'CPU', value: price?.cpu_hourly_pt },
    { label: '内存', value: price?.memory_hourly_pt },
    { label: '磁盘', value: price?.disk_hourly_pt },
    { label: 'GPU', value: price?.gpu_hourly_pt },
  ];
});
const REASONS: Record<string, string> = {
  credit_account_unavailable: '额度账户不可用',
  insufficient_credit: '可用额度不足以预留首个计费窗口',
  gpu_price_exceeds_limit: 'GPU 当前单价高于所设上限，请在套餐范围内调整价格上限，或选择不带 GPU',
  insufficient_disk_bytes: '节点可用磁盘不足',
  insufficient_gpu_capacity: '符合条件的 GPU 库存或可分配名额不足',
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
const blockingReasons = computed(() => matchedQuote.value?.blocking_reasons.map((reason) => REASONS[reason] ?? `暂不满足创建条件（${reason}），请联系管理员`) ?? []);
function networkLabel(item: NetworkOption) {
  const mode = { bridge: '桥接网络', macvlan: 'Macvlan 直连', managed_bridge: '托管桥接网络' }[item.mode] ?? item.mode;
  return `${mode} · 节点 ${item.node_id} · 项目 ${item.project_name}`;
}

// listAll 的默认页数有上限；选项超出安全上限时明确失败，而不是把截断结果当作完整目录。
async function loadOptions<T>(fetcher: (q: Query) => Promise<ApiResult<CursorPage<T>>>, query: Query): Promise<T[]> {
  let next: null | string = null;
  const seen = new Set<string>();
  const result = await listAll(async (q) => {
    const response = await fetcher(q);
    next = response.data.next_cursor;
    if (next && seen.has(next)) throw new Error('目录游标重复，请重试或联系管理员');
    if (next) seen.add(next);
    return response;
  }, query, 100);
  if (next) throw new Error('可选目录超过安全分页范围，请联系管理员缩小目录范围');
  return result;
}
function invalidateQuote() {
  ++quoteSequence;
  window.clearTimeout(quoteTimer);
  quote.value = null;
  quoteError.value = null;
  quoteLoading.value = false;
}
async function loadPlans() {
  if (locked.value || !identity.value) return;
  const token = session;
  const sequence = ++plansSequence;
  plansLoading.value = true;
  plansError.value = null;
  try {
    const result = await loadOptions(memberApi.plans, { instance_type: 'container' });
    if (!isCurrent(token) || sequence !== plansSequence) return;
    plans.value = result.filter((item) => item.instance_type === 'container' && item.status === 'active');
    if (!plans.value.some((item) => item.id === planId.value)) planId.value = plans.value[0]?.id;
  } catch (error) {
    if (isCurrent(token) && sequence === plansSequence) plansError.value = describeError(error);
  } finally {
    if (isCurrent(token) && sequence === plansSequence) plansLoading.value = false;
  }
}
async function loadNetworks() {
  if (locked.value || !identity.value) return;
  const token = session;
  const sequence = ++networksSequence;
  networksLoading.value = true;
  networksError.value = null;
  try {
    const result = await loadOptions(memberApi.networkOptions, { instance_type: 'container' });
    if (!isCurrent(token) || sequence !== networksSequence) return;
    networks.value = result;
  } catch (error) {
    if (isCurrent(token) && sequence === networksSequence) networksError.value = describeError(error);
  } finally {
    if (isCurrent(token) && sequence === networksSequence) networksLoading.value = false;
  }
}
function restoreImageChoice() {
  if (!preferredImageId || imagesLoading.value || imagesError.value) return;
  const preferred = availableImages.value.find((item) => item.id === preferredImageId && (!gpuCount.value || item.gpu_ready));
  imageId.value = preferred?.id;
  if (!preferred) confirmationNotice.value = '上次选择的镜像已不可用，请重新选择系统镜像。';
  preferredImageId = undefined;
}

function restoreSavedForm() {
  if (!pendingRestore || plansLoading.value || networksLoading.value || plansError.value || networksError.value) return;
  const saved = pendingRestore;
  pendingRestore = null;
  name.value = saved.name;
  const plan = plans.value.find((item) => item.id === saved.planId);
  if (!plan) {
    confirmationNotice.value = '上次选择的套餐已不可用，请重新选择套餐。';
    return;
  }
  preferredImageId = saved.imageId;
  planId.value = plan.id;
  resources.value = { ...saved.resources };
  resourcesValid.value = true;
  gpuCount.value = saved.gpuCount;
  gpuCap.value = saved.gpuCap;
  networkId.value = availableNetworks.value.some((item) => item.id === saved.networkId) ? saved.networkId : undefined;
  if (!networkId.value) {
    preferredImageId = undefined;
    imageId.value = undefined;
    confirmationNotice.value = '上次选择的网络已不可用，请重新选择网络和镜像。';
    return;
  }
  restoreImageChoice();
  if (orders.needsEdit) confirmationNotice.value = orders.notice || '订单配置已失效，请核对套餐、规格和最新报价后重新确认。';
}

async function loadImages() {
  const token = session;
  const sequence = ++imagesSequence;
  if (!preferredImageId) preferredImageId = imageId.value;
  images.value = [];
  imagesError.value = null;
  const network = selectedNetwork.value;
  if (!network || !identity.value) { imagesLoading.value = false; return; }
  imagesLoading.value = true;
  try {
    const result = await loadOptions(memberApi.images, { instance_type: 'container', node_id: network.node_id, project_name: network.project_name });
    if (!isCurrent(token) || sequence !== imagesSequence) return;
    // 指定节点及项目的目录响应已由服务端限定为 ready，并提供有效的 gpu_ready。
    images.value = result;
  } catch (error) {
    if (isCurrent(token) && sequence === imagesSequence) imagesError.value = describeError(error);
  } finally {
    if (isCurrent(token) && sequence === imagesSequence) {
      imagesLoading.value = false;
      restoreImageChoice();
    }
  }
}
async function refreshAccount() {
  if (!identity.value || document.hidden || accountLoading.value) return;
  const token = session;
  accountLoading.value = true;
  try {
    const response = await memberApi.creditAccount();
    if (!isCurrent(token)) return;
    account.value = response.data;
    accountError.value = null;
  } catch (error) {
    if (isCurrent(token)) accountError.value = describeError(error);
  } finally {
    if (isCurrent(token)) accountLoading.value = false;
  }
}
async function reloadForConfirmation() {
  confirmationNotice.value = '套餐或政策已变更，正在重新加载配置和报价；请核对后再进入订单确认。';
  invalidateQuote();
  await Promise.all([loadPlans(), loadNetworks(), refreshAccount()]);
}
async function requestQuote() {
  if (!spec.value || locked.value) return;
  const snapshot = spec.value;
  const snapshotFingerprint = fingerprint.value;
  const token = session;
  invalidateQuote();
  const sequence = quoteSequence;
  quoteLoading.value = true;
  try {
    const response = await memberApi.quote(snapshot, { key: newIdempotencyKey() });
    if (!isCurrent(token) || sequence !== quoteSequence || fingerprint.value !== snapshotFingerprint) return;
    if (response.data.plan_revision !== snapshot.plan_revision) {
      await reloadForConfirmation();
      return;
    }
    quote.value = { fingerprint: snapshotFingerprint, value: response.data };
    now.value = Date.now();
  } catch (error) {
    if (!isCurrent(token) || sequence !== quoteSequence) return;
    quoteError.value = describeError(error);
    if (error instanceof ApiError && ['plan_changed', 'policy_changed'].includes(error.code)) {
      changedError.value = quoteError.value;
      await reloadForConfirmation();
    }
  } finally {
    if (isCurrent(token) && sequence === quoteSequence) quoteLoading.value = false;
  }
}
async function proceedToOrder() {
  if (locked.value || !identity.value) return;
  const current = matchedQuote.value;
  if (current && !(Date.parse(current.expires_at) > Date.now())) { await requestQuote(); return; }
  if (!canCreate.value || !spec.value || !current || !selectedPlan.value || !selectedImage.value || !selectedNetwork.value) return;
  orders.saveForm({
    planId: planId.value,
    networkId: networkId.value,
    imageId: imageId.value,
    name: name.value,
    resources: { ...resources.value },
    gpuCount: gpuCount.value,
    gpuCap: gpuCap.value,
  });
  preparingOrder.value = true;
  const token = session;
  try {
    const prepared = await orders.prepare({
      spec: { ...spec.value, name: name.value.trim() },
      plan: selectedPlan.value,
      image: selectedImage.value,
      network: selectedNetwork.value,
      quote: current,
      account: account.value,
    });
    if (prepared && isCurrent(token)) await router.push('/instances/create/confirm');
  } finally {
    if (isCurrent(token)) preparingOrder.value = false;
  }
}

watch(loginDraft, async (value) => {
  if (disposed) return;
  loginMemory.save(value);
  const sequence = ++loginSequence;
  loginValidating.value = true;
  const result = await validateLoginDraft({ ...value });
  if (disposed || sequence !== loginSequence) return;
  loginError.value = result.error;
  loginValidating.value = false;
}, { deep: true, immediate: true, flush: 'sync' });

watch(selectedPlan, (plan) => {
  resources.value = plan ? { ...plan.resources } : { disk_bytes: '0', memory_bytes: '0', vcpus: 0 };
  resourcesValid.value = true;
  gpuCount.value = plan?.gpu.count ?? 0;
  gpuCap.value = plan?.gpu.max_hourly_pt ?? '0';
}, { flush: 'sync' });
watch(availableNetworks, (options) => {
  if (!options.some((item) => item.id === networkId.value)) networkId.value = options.length === 1 ? options[0]?.id : undefined;
}, { flush: 'sync' });
watch([selectedPlan, selectedNetwork], () => { void loadImages(); }, { flush: 'sync' });
watch([fingerprint, resourcesValid, selectedPlan, networkId, imageId, gpuCount, gpuCap], () => {
  invalidateQuote();
  if (spec.value && !locked.value) quoteTimer = window.setTimeout(() => { void requestQuote(); }, 400);
}, { deep: true, flush: 'sync' });
watch([plans, networks, plansLoading, networksLoading, plansError, networksError], restoreSavedForm, { flush: 'post' });
watch(locked, (value) => {
  if (value && identity.value) void router.replace('/instances/create/confirm');
}, { immediate: true });
watch(identity, () => {
  ++session;
  ++plansSequence;
  ++networksSequence;
  ++imagesSequence;
  invalidateQuote();
  pendingRestore = identity.value && orders.form ? { ...orders.form, resources: { ...orders.form.resources } } : null;
  preferredImageId = undefined;
  plans.value = [];
  networks.value = [];
  images.value = [];
  planId.value = undefined;
  networkId.value = undefined;
  imageId.value = undefined;
  name.value = '';
  loginDraft.value = identity.value ? loginMemory.read() : emptyLoginDraft();
  preparingOrder.value = false;
  account.value = null;
  accountError.value = null;
  accountLoading.value = false;
  changedError.value = null;
  confirmationNotice.value = '';
  if (identity.value) void Promise.all([loadPlans(), loadNetworks(), refreshAccount()]);
}, { immediate: true, flush: 'sync' });
usePolling(refreshAccount, () => 30_000);
onBeforeUnmount(() => {
  disposed = true;
  ++session;
  ++loginSequence;
  loginDraft.value = emptyLoginDraft();
  invalidateQuote();
  window.clearInterval(clock);
});
</script>

<template>
  <Page title="创建弹性容器" description="选择套餐与资源后进入订单确认；本页不会立即创建实例。">
    <div class="mb-4 flex items-center justify-between gap-3">
      <Button :disabled="locked" @click="router.push('/instances')">返回实例列表</Button>
      <Tag color="blue" class="m-0">弹性容器 · 额度计费</Tag>
    </div>
    <Alert v-if="confirmationNotice" class="mb-4" :message="confirmationNotice" show-icon type="warning">
      <template v-if="changedError" #description><ErrorFeedback :feedback="changedError" /></template>
    </Alert>
    <div class="creation-grid">
      <div class="creation-main min-w-0">
        <Card title="选择套餐" size="small">
          <template #extra><Button size="small" :disabled="locked" :loading="plansLoading" @click="loadPlans">刷新套餐</Button></template>
          <ErrorFeedback v-if="plansError" :feedback="plansError" />
          <Skeleton v-else-if="plansLoading && !plans.length" active :paragraph="{ rows: 3 }" />
          <Empty v-else-if="!plans.length" description="暂无已启用的容器套餐">
            <Button :disabled="locked" @click="loadPlans">重新加载套餐</Button>
          </Empty>
          <div v-else class="plan-grid" role="group" aria-label="选择容器套餐">
            <button v-for="plan in plans" :key="plan.id" type="button" class="plan-option" :class="{ 'is-selected': planId === plan.id }" :aria-pressed="planId === plan.id" :disabled="locked || plansLoading" @click="planId = plan.id">
              <span class="flex items-center justify-between gap-2"><strong>{{ plan.name }}</strong><Tag v-if="planId === plan.id" color="blue" class="m-0">已选</Tag></span>
              <span class="text-muted-foreground mt-2 block text-xs">{{ plan.resources.vcpus }} vCPU · {{ formatBytes(plan.resources.memory_bytes) }} 内存 · {{ formatBytes(plan.resources.disk_bytes) }} 磁盘</span>
              <span class="text-muted-foreground mt-1 block text-xs">{{ plan.gpu.model ? `${plan.gpu.model} · ${plan.gpu.access_mode === 'shared' ? '共享' : '独占'}` : '不带 GPU 的基础资源' }}</span>
            </button>
          </div>
          <p class="text-muted-foreground mb-0 mt-3 text-xs">套餐切换后资源恢复该套餐基准，费用以右侧实时服务端报价为准。</p>
        </Card>

        <Card v-if="selectedPlan" title="资源配置" size="small">
          <ResourceConfigurator v-model="resources" :plan="selectedPlan" :disabled="locked || plansLoading" @validity-change="resourcesValid = $event" />
          <Alert v-if="resourceError" class="mt-3" :message="resourceError" show-icon type="warning" />
        </Card>

        <Card v-if="selectedPlan" title="GPU 与网络" size="small">
          <div class="space-y-4">
            <div>
              <div class="mb-2 flex flex-wrap items-center gap-2"><strong>GPU</strong><Tag>{{ selectedPlan.gpu.model || '不使用 GPU' }}</Tag><Tag v-if="selectedPlan.gpu.model">{{ selectedPlan.gpu.access_mode === 'shared' ? '共享' : '独占' }}</Tag></div>
              <p class="text-muted-foreground mb-3 text-xs">型号与共享方式由套餐固定。{{ gpuRangeText }}，任何套餐均可选择不带 GPU。</p>
              <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
                <label class="field-label"><span>GPU 数量（张）</span><InputNumber v-model:value="gpuCount" class="!w-full" :min="0" :max="gpuMaximum" :precision="0" :disabled="locked || !selectedPlan.gpu.model" aria-label="GPU 数量" /></label>
                <label class="field-label"><span>GPU 每小时价格上限（Pt）</span><Input v-model:value="gpuCap" inputmode="decimal" :disabled="locked" aria-label="GPU 每小时价格上限" /></label>
              </div>
              <p class="text-muted-foreground mb-0 mt-2 text-xs">最多 {{ formatPt(selectedPlan.gpu.max_hourly_pt) }} Pt/小时，可调低；价格上限不是实际报价，调低可能导致无可用 GPU。</p>
              <p v-if="gpuError" class="text-destructive mb-0 mt-2 text-sm" role="alert">{{ gpuError }}</p>
            </div>
            <div class="border-border border-t pt-4">
              <div class="mb-2 flex items-center justify-between gap-3"><label for="creation-network" class="font-medium">网络</label><Button size="small" :disabled="locked" :loading="networksLoading" @click="loadNetworks">刷新网络</Button></div>
              <ErrorFeedback v-if="networksError" :feedback="networksError" />
              <Empty v-else-if="!networksLoading && !availableNetworks.length" description="此套餐暂无可用网络">
                <Button :disabled="locked" @click="loadNetworks">重新加载网络</Button>
              </Empty>
              <Select v-else id="creation-network" v-model:value="networkId" class="w-full" :disabled="locked || networksLoading" :loading="networksLoading" :options="availableNetworks.map((item) => ({ label: networkLabel(item), value: item.id }))" placeholder="请选择网络" />
              <p class="text-muted-foreground mb-0 mt-2 text-xs">网络决定容器所在节点与项目，镜像仅展示该位置已就绪的套餐镜像。</p>
            </div>
          </div>
        </Card>
      </div>

      <div class="creation-sidebar min-w-0">
        <Card title="系统镜像与名称" size="small">
          <div class="space-y-4">
            <div>
              <div class="mb-2 flex items-center justify-between gap-3"><span class="font-medium">系统镜像</span><Button size="small" :disabled="locked || !selectedNetwork" :loading="imagesLoading" @click="loadImages">刷新镜像</Button></div>
              <p v-if="!selectedNetwork" class="text-muted-foreground mb-0 text-sm">请先选择套餐和网络，再选择该节点、项目下的就绪镜像。</p>
              <ErrorFeedback v-else-if="imagesError" :feedback="imagesError" />
              <Skeleton v-else-if="imagesLoading" active :paragraph="{ rows: 2 }" />
              <Empty v-else-if="!availableImages.length" description="所选网络下暂无套餐允许的就绪镜像"><Button :disabled="locked" @click="loadImages">重新加载镜像</Button></Empty>
              <ImageSelector v-else v-model="imageId" :images="availableImages" :gpu-count="gpuCount ?? 0" :disabled="locked" />
            </div>
            <label class="field-label"><span>实例名称</span><Input v-model:value="name" :disabled="locked" :maxlength="128" placeholder="为此容器起一个便于识别的名称" aria-label="实例名称" /><span v-if="name && nameError" class="text-destructive text-xs">{{ nameError }}</span><span class="text-muted-foreground text-xs">创建时必填，1–128 个字符；不影响报价与时长估算。</span></label>
            <!-- <Alert v-if="selectedPlan?.init_template_version_id" message="此套餐使用管理员预设的初始化模板" description="初始化参数为 {}。" show-icon type="info" /> -->
          </div>
        </Card>

        <Card title="登录方式" size="small">
          <div class="space-y-4">
            <RadioGroup id="creation-login-mode" v-model:value="loginDraft.mode" :disabled="locked || preparingOrder" aria-label="登录方式">
              <RadioButton value="password">root 密码</RadioButton>
              <RadioButton value="ssh-key">SSH 公钥</RadioButton>
            </RadioGroup>
            <template v-if="loginDraft.mode === 'password'">
              <label class="field-label"><span>root 密码</span><InputPassword id="creation-root-password" v-model:value="loginDraft.rootPassword" :disabled="locked || preparingOrder" autocomplete="new-password" aria-label="root 密码" aria-describedby="creation-password-help creation-login-error" /></label>
              <label class="field-label"><span>确认 root 密码</span><InputPassword id="creation-root-password-confirmation" v-model:value="loginDraft.rootPasswordConfirmation" :disabled="locked || preparingOrder" autocomplete="new-password" aria-label="确认 root 密码" aria-describedby="creation-login-error" /></label>
              <p id="creation-password-help" class="text-muted-foreground mb-0 text-xs">8–128 个字符，不含控制字符；空格原样保留。</p>
            </template>
            <template v-else>
              <label class="field-label" for="creation-ssh-public-keys"><span>SSH 公钥</span><Textarea id="creation-ssh-public-keys" v-model:value="loginDraft.sshPublicKeyText" :disabled="locked || preparingOrder" :auto-size="{ minRows: 3, maxRows: 8 }" class="font-mono text-xs" autocomplete="off" :spellcheck="false" placeholder="ssh-ed25519 AAAA…" aria-label="SSH 公钥" aria-describedby="creation-ssh-keys-help creation-login-error" /></label>
              <p id="creation-ssh-keys-help" class="text-muted-foreground mb-0 text-xs">每行一条，1–10 条，每条最多 8192 字节；支持 Ed25519、RSA ≥2048 位、ECDSA nistp256/384/521。</p>
            </template>
            <p v-if="loginValidating" class="text-muted-foreground mb-0 text-xs" role="status">正在校验登录凭据…</p>
            <p v-else-if="loginError" id="creation-login-error" class="text-destructive mb-0 text-xs" role="alert">{{ loginError }}</p>
            <Alert v-if="loginImageError" :message="loginImageError" show-icon type="warning" />
            <p class="text-muted-foreground mb-0 text-xs">SSH 服务需自行安装。</p>
          </div>
        </Card>

        <Card title="实时报价" size="small" class="quote-card">
          <template #extra><Button size="small" :disabled="!canRefreshQuote" :loading="quoteLoading" @click="requestQuote">重新报价</Button></template>
          <div aria-live="polite" :aria-busy="quoteLoading" class="space-y-4">
            <p v-if="quoteLoading" class="text-muted-foreground mb-0 text-sm">正在获取最新报价，旧报价已失效…</p>
            <p v-else-if="!matchedQuote && !quoteError" class="text-muted-foreground mb-0 text-sm">{{ spec ? '配置已更新，等待实时服务端报价…' : '请完整选择套餐、资源、网络和镜像后获取报价，无需先填写名称或登录凭据。' }}</p>
            <ErrorFeedback v-if="quoteError" :feedback="quoteError" />
            <dl class="m-0 space-y-2">
              <div v-for="row in priceRows" :key="row.label" class="price-row"><dt class="text-muted-foreground">{{ row.label }}</dt><dd>{{ formatPt(row.value) }} Pt/小时</dd></div>
              <div class="price-row border-border border-t pt-3"><dt class="font-medium">合计</dt><dd class="text-primary text-xl font-semibold">{{ formatPt(matchedQuote?.price_details.total_hourly_pt) }} <span class="text-muted-foreground text-xs font-normal">Pt/小时</span></dd></div>
              <div class="price-row"><dt class="text-muted-foreground">首窗口预留</dt><dd>{{ formatPt(matchedQuote?.initial_window_pt) }} Pt</dd></div>
            </dl>
            <p v-if="matchedQuote" class="text-muted-foreground mb-0 text-xs">报价{{ quoteExpired ? '已过期' : `有效至 ${formatTime(matchedQuote.expires_at)}` }}；报价不预留资源，也不是创建承诺。</p>
            <Alert v-if="quoteExpired" message="报价已过期，请重新报价并再次确认，不能直接提交旧报价。" show-icon type="warning" />
            <Alert v-if="blockingReasons.length || (matchedQuote && !matchedQuote.admissible)" message="当前配置暂不能创建" show-icon type="warning"><template #description><ul class="mb-0 pl-4"><li v-for="reason in blockingReasons" :key="reason">{{ reason }}</li><li v-if="!blockingReasons.length">服务端暂不允许创建，请重新报价或联系管理员。</li></ul></template></Alert>
            <div class="border-border border-t pt-4">
              <div class="mb-2 flex items-center justify-between gap-3"><span class="font-medium">如果只运行该实例预计可用时长</span><Button size="small" :loading="accountLoading" @click="refreshAccount">刷新额度</Button></div>
              <p class="mb-2 text-xl font-semibold">{{ runtime }}</p>
              <p class="text-muted-foreground mb-2 text-sm">当前可用额度：<span class="font-mono">{{ formatPt(account?.available_pt) }} Pt</span></p>
              <ErrorFeedback v-if="accountError" :feedback="accountError" />
              <Alert v-else-if="accountBlock" :message="accountBlock" show-icon type="warning" />
              <!-- <p class="text-muted-foreground mb-0 mt-2 text-xs leading-relaxed">仅按此实例的当前报价与可用余额静态估算；可用余额已扣除预留及欠款，不再重复扣除。不包含其他实例消耗、未来额度发放或额度到期，也不保证连续运行。零费率不表示无限时长。</p> -->
            </div>
            <div class="desktop-submit"><Button type="primary" block size="large" :disabled="primaryDisabled" @click="proceedToOrder">{{ primaryLabel }}</Button><p class="text-muted-foreground mb-0 mt-2 text-center text-xs">下一步核对配置与费用，不会立即创建</p></div>
          </div>
        </Card>
      </div>
    </div>
    <div class="mobile-submit">
      <div class="min-w-0 flex-1"><p class="m-0 font-semibold">{{ formatPt(matchedQuote?.price_details.total_hourly_pt) }} <span class="text-muted-foreground text-xs font-normal">Pt/小时</span></p><p class="text-muted-foreground m-0 truncate text-xs">预计 {{ runtime }}</p></div>
      <Button type="primary" :disabled="primaryDisabled" @click="proceedToOrder">{{ primaryLabel }}</Button>
    </div>
  </Page>
</template>

<style scoped>
.creation-grid { display: grid; grid-template-columns: minmax(0, 1fr); gap: 1rem; padding-bottom: 5.5rem; }
.creation-main, .creation-sidebar { display: flex; flex-direction: column; gap: 1rem; }
.plan-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(min(100%, 15rem), 1fr)); gap: 0.75rem; }
.plan-option { width: 100%; padding: 0.9rem; border: 1px solid hsl(var(--border)); border-radius: 0.65rem; background: hsl(var(--card)); color: inherit; text-align: left; cursor: pointer; transition: border-color 150ms ease, background-color 150ms ease; }
.plan-option:hover:not(:disabled), .is-selected { border-color: hsl(var(--primary)); background: hsl(var(--primary) / 0.05); }
.plan-option:focus-visible { outline: 2px solid hsl(var(--primary)); outline-offset: 2px; }
.plan-option:disabled { cursor: not-allowed; opacity: 0.6; }
.field-label { display: flex; flex-direction: column; gap: 0.45rem; font-size: 0.875rem; }
.price-row { display: flex; align-items: baseline; justify-content: space-between; gap: 1rem; }
.price-row dd { margin: 0; text-align: right; overflow-wrap: anywhere; font-variant-numeric: tabular-nums; }
.mobile-submit { position: fixed; right: 0; bottom: 0; left: 0; z-index: 30; display: flex; align-items: center; gap: 0.75rem; padding: 0.75rem max(1rem, env(safe-area-inset-right)) max(0.75rem, env(safe-area-inset-bottom)) max(1rem, env(safe-area-inset-left)); border-top: 1px solid hsl(var(--border)); background: hsl(var(--background)); }
.desktop-submit { display: none; }
@media (min-width: 1024px) {
  .creation-grid { grid-template-columns: minmax(0, 1.2fr) minmax(22rem, 0.8fr); align-items: start; padding-bottom: 1rem; }
  .creation-sidebar { align-self: stretch; }
  .quote-card { position: sticky; top: 1rem; }
  .desktop-submit { display: block; }
  .mobile-submit { display: none; }
}
@media (prefers-reduced-motion: reduce) { .plan-option { transition: none; } }
</style>
