<script lang="ts" setup>
import type { PolicyVersion } from '#/api/types';

import { computed, nextTick, onMounted, ref } from 'vue';

import { Select } from 'ant-design-vue';

import { listAll, policiesApi } from '#/api/backend';
import { errorToast } from '#/composables/api';

const value = defineModel<string>();
const profiles = ref<PolicyVersion[]>([]);
const loading = ref(false);
const options = computed(() => profiles.value
  .filter((p) => p.status === 'active' && !!p.effective_at && Date.parse(p.effective_at) <= Date.now())
  .map((p) => ({ label: `${p.code} v${p.version}`, value: p.id })));

async function validate() {
  // Select triggers form validation before this wrapper receives its updated model prop.
  await nextTick();
  if (!options.value.some((p) => p.value === value.value)) {
    return Promise.reject(new Error('请选择已发布且已生效的监控策略'));
  }
  return Promise.resolve();
}
defineExpose({ validate });

onMounted(async () => {
  loading.value = true;
  try {
    profiles.value = await listAll((q) => policiesApi.list('metrics', q));
  } catch (error) {
    errorToast(error);
  } finally {
    loading.value = false;
  }
});
</script>

<template>
  <Select
    v-model:value="value"
    :loading="loading"
    :options="options"
    placeholder="请选择已发布且已生效的监控策略"
    not-found-content="没有可用的监控策略，请先发布策略"
    show-search
    option-filter-prop="label"
  />
</template>
