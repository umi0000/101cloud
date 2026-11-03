<script lang="ts" setup>
import type { Query } from '#/api/backend';
import type { Instance } from '#/api/types';
import type { ErrorText } from '#/locales/errors';

import { computed, onMounted, ref, shallowRef, watch } from 'vue';
import { useRouter } from 'vue-router';

import { Page } from '@vben/common-ui';

import { Alert, Button, Card, Empty, Select, Skeleton } from 'ant-design-vue';

import { memberApi } from '#/api/backend';
import CursorPager from '#/components/cursor-pager.vue';
import ErrorFeedback from '#/components/error-feedback.vue';
import { useCursorList, usePolling } from '#/composables/api';
import { ENUMS } from '#/locales/enums';
import { describeError } from '#/locales/errors';
import { useTaskStore } from '#/store/tasks';

import InstanceCard from './components/instance-card.vue';

type Filters = Record<'observed_state' | 'product_type', string | undefined>;
const router = useRouter();
const tasks = useTaskStore();
const filters = ref<Filters>({ observed_state: undefined, product_type: undefined });
const loadError = shallowRef<ErrorText | null>(null);
const hasLoaded = ref(false);
const loadedPage = ref(1);
const gpuRefreshKey = ref(0);
let fetchSequence = 0;

// useCursorList 保留旧数据；另记错误与成功页码，避免把失败误报为空列表。
async function fetchInstances(query: Query) {
  const sequence = ++fetchSequence;
  const requestedPage = list.page();
  try {
    const response = await memberApi.instances(query);
    if (sequence === fetchSequence) {
      loadError.value = null;
      hasLoaded.value = true;
      loadedPage.value = requestedPage;
    }
    return response;
  } catch (error) {
    if (sequence === fetchSequence) {
      loadError.value = describeError(error);
      gpuRefreshKey.value++;
    }
    throw error;
  }
}

const list = useCursorList<Instance, Filters>(fetchInstances, filters, 20);
const productOptions = Object.entries(ENUMS.productType ?? {}).map(([value, label]) => ({ label: label.text, value }));
const stateOptions = Object.entries(ENUMS.observed ?? {}).map(([value, label]) => ({ label: label.text, value }));
const filtered = computed(() => !!(filters.value.observed_state || filters.value.product_type));
const busy = computed(() => list.items.value.some((instance) => instance.active_task_id
  || instance.provisioning_state !== 'ready' || instance.observed_state === 'pending'
  || ['grace', 'stopping', 'unknown'].includes(instance.credit_enforcement.status)));

usePolling(() => {
  if (!list.loading.value) return list.load();
}, () => (busy.value ? 10_000 : 30_000));
onMounted(() => void list.load());
watch(
  () => tasks.tracked.filter((item) => item.task && ['succeeded', 'failed', 'cancelled'].includes(item.task.status))
    .map((item) => `${item.taskId}:${item.task?.status}`).join('|'),
  () => void list.load(),
);
</script>

<template>
  <Page title="实例管理">
    <Card class="member-toolbar" size="small">
      <div class="flex flex-wrap items-end gap-3">
        <label class="flex min-w-0 flex-1 flex-col gap-1 sm:flex-none">
          <span class="text-muted-foreground text-xs">运行状态</span>
          <Select
            v-model:value="filters.observed_state"
            allow-clear
            aria-label="筛选运行状态"
            class="w-full sm:w-40"
            :options="stateOptions"
            placeholder="全部状态"
          />
        </label>
        <label class="flex min-w-0 flex-1 flex-col gap-1 sm:flex-none">
          <span class="text-muted-foreground text-xs">产品类型</span>
          <Select
            v-model:value="filters.product_type"
            allow-clear
            aria-label="筛选产品类型"
            class="w-full sm:w-40"
            :options="productOptions"
            placeholder="全部类型"
          />
        </label>
        <Button :loading="list.loading.value" @click="list.load">刷新</Button>
        <Button class="sm:ml-auto" type="primary" @click="router.push('/instances/create')">
          创建实例
        </Button>
      </div>
    </Card>

    <Alert v-if="loadError" class="mb-4" message="实例列表加载失败" show-icon type="error">
      <template #description>
        <ErrorFeedback :feedback="loadError" />
        <p v-if="hasLoaded" class="mt-2 mb-0 text-sm">
          下方保留上次成功加载的结果（第 {{ loadedPage }} 页），本次筛选或翻页尚未应用。请刷新重试。
        </p>
        <p v-else class="mt-2 mb-0 text-sm">暂时无法获取实例，请刷新重试。</p>
      </template>
    </Alert>

    <div v-if="!hasLoaded && !loadError" aria-busy="true" aria-label="正在加载实例" class="instance-grid">
      <Card v-for="index in 3" :key="index" size="small">
        <Skeleton active :paragraph="{ rows: 6 }" />
      </Card>
    </div>
    <div v-else-if="list.items.value.length" :aria-busy="list.loading.value" class="instance-grid">
      <InstanceCard
        v-for="instance in list.items.value"
        :key="instance.id"
        :instance="instance"
        :refresh-key="gpuRefreshKey"
      />
    </div>
    <Card v-else-if="hasLoaded && !loadError" size="small">
      <Empty :description="filtered ? '没有符合筛选条件的实例' : '暂无实例'" />
    </Card>

    <div class="min-w-0 overflow-x-auto">
      <CursorPager
        :has-next="!loadError && list.hasNext()"
        :has-prev="!loadError && list.hasPrev()"
        :loading="list.loading.value"
        :page="loadedPage"
        @next="list.nextPage"
        @prev="list.prevPage"
        @refresh="list.load"
      />
    </div>
  </Page>
</template>

<style scoped>
.member-toolbar {
  margin-bottom: 1rem;
}

.instance-grid {
  display: grid;
  grid-template-columns: minmax(0, 1fr);
  gap: 1rem;
}

@media (min-width: 768px) {
  .instance-grid {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }
}

@media (min-width: 1536px) {
  .instance-grid {
    grid-template-columns: repeat(3, minmax(0, 1fr));
  }
}
</style>
