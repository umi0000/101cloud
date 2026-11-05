<script lang="ts" setup>
import type { GPUAllocation, Instance, InstanceNetwork } from '#/api/types';
import type { EnumLabel } from '#/locales/enums';
import type { ErrorText } from '#/locales/errors';

import { computed, onBeforeUnmount, ref, shallowRef, watch } from 'vue';
import { useRouter } from 'vue-router';

import { Button, Card, Spin, Tag } from 'ant-design-vue';

import { memberApi } from '#/api/backend';
import ErrorFeedback from '#/components/error-feedback.vue';
import OsIcon from '#/components/os-icon.vue';
import StatusTag from '#/components/status-tag.vue';
import { enumLabel, instanceState } from '#/locales/enums';
import { describeError } from '#/locales/errors';
import { formatBytes, formatFullTime } from '#/utils/format';
import { gpuAttachmentState } from '#/utils/gpu-attachment';
import { isPrivateIPv4 } from '#/utils/private-ipv4';

const props = defineProps<{ instance: Instance; refreshKey: number }>();
const emit = defineEmits<{ view: [instance: Instance] }>();
const router = useRouter();
const allocations = shallowRef<GPUAllocation[]>([]);
const gpuLoading = ref(false);
const gpuLoaded = ref(false);
const gpuError = shallowRef<ErrorText | null>(null);
let sequence = 0;
const networks = shallowRef<InstanceNetwork[]>([]);
const networkLoading = ref(false);
const networkLoaded = ref(false);
const networkError = shallowRef<ErrorText | null>(null);
let networkSequence = 0;

const privateNetworks = computed(() => networks.value.filter((network) =>
  network.status === 'bound' && network.quality.configuration_bound && isPrivateIPv4(network.ip)));

async function loadNetwork() {
  const mine = ++networkSequence;
  networkLoading.value = true;
  try {
    const response = await memberApi.network(props.instance.id);
    if (mine !== networkSequence) return;
    networks.value = response.data;
    networkLoaded.value = true;
    networkError.value = null;
  } catch (error) {
    if (mine === networkSequence) networkError.value = describeError(error);
  } finally {
    if (mine === networkSequence) networkLoading.value = false;
  }
}

async function loadGPU() {
  const mine = ++sequence;
  gpuLoading.value = true;
  try {
    const response = await memberApi.gpuAllocations(props.instance.id);
    if (mine !== sequence) return;
    allocations.value = response.data;
    gpuLoaded.value = true;
    gpuError.value = null;
  } catch (error) {
    if (mine === sequence) gpuError.value = describeError(error);
  } finally {
    if (mine === sequence) gpuLoading.value = false;
  }
}

watch(() => [props.instance, props.refreshKey], () => {
  void loadGPU();
  void loadNetwork();
}, { immediate: true });
onBeforeUnmount(() => {
  sequence++;
  networkSequence++;
});

// 释放和取消是历史记录；失败、释放中、未知仍需如实呈现，不能视作已挂载。
const currentAllocations = computed(() => allocations.value.filter((allocation) =>
  allocation.status !== 'released' && allocation.status !== 'cancelled'));


const gpuGroups = computed(() => {
  const groups = new Map<string, { count: number; label: EnumLabel; model: string }>();
  for (const allocation of currentAllocations.value) {
    const label = enumLabel('gpuAttachment', gpuAttachmentState(allocation));
    const key = JSON.stringify([allocation.model, label.text]);
    const group = groups.get(key);
    if (group) group.count++;
    else groups.set(key, { count: 1, label, model: allocation.model });
  }
  return [...groups.entries()];
});

const enforcementMessage = computed(() => {
  const enforcement = props.instance.credit_enforcement;
  switch (enforcement.status) {
    case 'grace': {
      return enforcement.shutdown_deadline
        ? `额度不足，宽限停机时间：${formatFullTime(enforcement.shutdown_deadline)}`
        : '额度不足，处于停机宽限期，停机期限待确认。';
    }
    case 'stopping': {
      return '额度不足，正在执行停机；请以更新后的实例状态为准。';
    }
    case 'unknown': {
      return '额度停机状态待核对，请留意后续状态更新。';
    }
    case 'completed': {
      return '额度停机流程已完成，请以实例状态为准。';
    }
    default: {
      return null;
    }
  }
});
</script>

<template>
  <Card class="instance-card h-full min-w-0" size="small">
    <article class="flex min-w-0 flex-col gap-5" :aria-label="`实例 ${instance.name}`">
      <header class="flex min-w-0 items-start gap-3">
        <div class="flex size-10 shrink-0 items-center justify-center rounded-lg" aria-hidden="true">
          <OsIcon :name="instance.image_os_name" class="text-2xl" />
        </div>
        <div class="flex min-w-0 flex-1 flex-col gap-2">
          <h2 class="m-0 break-words text-lg font-semibold [overflow-wrap:anywhere]">{{ instance.name }}</h2>
          <p class="text-muted-foreground m-0 break-all text-xs">
            <template v-if="instance.image_os_name">{{ instance.image_os_name }} {{ instance.image_os_release }} · </template>
            {{ enumLabel('instanceType', instance.instance_type).text }} · ID：{{ instance.id }}
          </p>
          <div class="flex flex-wrap items-center gap-2">
            <StatusTag :label="instanceState(instance)" />
            <Tag v-if="instance.active_task_id" class="m-0" color="processing">操作中</Tag>
            <StatusTag kind="productType" :value="instance.product_type" />
          </div>
        </div>
      </header>

      <dl class="m-0 grid min-w-0 grid-cols-3 gap-2">
        <div class="min-w-0">
          <dt class="text-muted-foreground text-xs">CPU</dt>
          <dd class="m-0 mt-1 break-words font-medium tabular-nums">{{ instance.resources.vcpus }} vCPU</dd>
        </div>
        <div class="min-w-0">
          <dt class="text-muted-foreground text-xs">内存</dt>
          <dd class="m-0 mt-1 break-words font-medium tabular-nums">{{ formatBytes(instance.resources.memory_bytes) }}</dd>
        </div>
        <div class="min-w-0">
          <dt class="text-muted-foreground text-xs">磁盘</dt>
          <dd class="m-0 mt-1 break-words font-medium tabular-nums">{{ formatBytes(instance.resources.disk_bytes) }}</dd>
        </div>
      </dl>

      <section class="min-w-0" aria-label="内网 IPv4">
        <h3 class="text-muted-foreground m-0 mb-1 text-xs font-normal">内网 IPv4</h3>
        <p v-if="networkLoading && !networkLoaded" class="text-muted-foreground m-0 text-sm" role="status">正在加载…</p>
        <ul v-if="privateNetworks.length" class="m-0 list-none space-y-1 p-0">
          <li v-for="network in privateNetworks" :key="network.ip" class="flex flex-wrap items-center gap-2">
            <span class="select-text break-all font-mono text-sm">{{ network.ip }}</span>
            <span v-if="network.quality.state_stale" class="text-muted-foreground text-xs">状态已过期</span>
          </li>
        </ul>
        <p v-else-if="networkLoaded && !networkError" class="text-muted-foreground m-0 text-sm">未分配内网 IPv4</p>
        <div v-if="networkError" class="mt-1 min-w-0" role="status">
          <p class="text-destructive m-0 text-xs">内网 IPv4 加载失败{{ networkLoaded ? '，保留上次结果' : '' }}；请刷新重试。</p>
          <ErrorFeedback :feedback="networkError" />
        </div>
      </section>

      <p v-if="instance.state_stale" class="text-muted-foreground m-0 break-words text-xs" role="status">
        状态信息已过期，暂不能确认实例当前运行状态。
        <span v-if="instance.observed_at">最近观测：{{ formatFullTime(instance.observed_at) }}</span>
        <span v-else>暂无有效观测。</span>
      </p>
      <p v-if="enforcementMessage" class="text-destructive m-0 break-words text-xs" role="status">
        {{ enforcementMessage }}
      </p>

      <section class="min-w-0 border-t pt-3" aria-label="GPU 分配">
        <h3 class="m-0 mb-2 text-sm font-medium">GPU 分配</h3>
        <div v-if="gpuLoading" class="text-muted-foreground mb-2 flex items-center gap-2 text-xs" role="status">
          <Spin size="small" />
          <span>{{ gpuLoaded ? '正在更新 GPU 分配…' : '正在加载 GPU 分配…' }}</span>
        </div>
        <div v-if="gpuError" class="mb-2 min-w-0" role="status">
          <p class="text-destructive m-0 mb-1 text-xs">GPU 分配加载失败{{ gpuLoaded ? '，以下保留上次成功加载的结果' : '' }}；请刷新重试。</p>
          <ErrorFeedback :feedback="gpuError" />
        </div>
        <p v-if="gpuLoaded && !currentAllocations.length" class="text-muted-foreground m-0 text-sm">未挂载 GPU</p>
        <ul v-else-if="currentAllocations.length" class="m-0 min-w-0 list-none space-y-3 p-0">
          <li v-for="[key, group] in gpuGroups" :key="key" class="flex min-w-0 flex-wrap items-center gap-2">
            <span class="min-w-0 break-words text-sm font-medium [overflow-wrap:anywhere]">{{ group.model }} × {{ group.count }}</span>
            <StatusTag :label="group.label" />
          </li>
        </ul>
      </section>

      <footer class="flex flex-wrap gap-2 border-t pt-3">
        <Button size="small" @click="emit('view', instance)">进入实例</Button>
        <Button v-if="instance.creation_task_id" size="small" @click="router.push({ name: 'MemberInstanceInitialization', params: { taskId: instance.creation_task_id } })">查看初始化</Button>
      </footer>
    </article>
  </Card>
</template>

<style scoped>
.instance-card :deep(.ant-card-body) {
  padding: 1.25rem;
}
</style>
