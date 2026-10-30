<script lang="ts" setup>
/**
 * 发布草稿 POST .../publish {effective_at}（+08:00，If-Match 用详情 ETag）。
 * 对比对象：同 kind 的 active 版本。一个 kind 可能同时有多个 active 版本，默认选平台绑定的版本；
 * GPU 定价按同型号同使用方式匹配；否则取已生效中 effective_at 最新的。管理员可改选。
 */
import type { Dayjs } from 'dayjs';

import type { Plan, PolicyKind, PolicyVersion } from '#/api/types';

import { computed, ref, watch } from 'vue';

import { Alert, DatePicker, Empty, Modal, Select, Table } from 'ant-design-vue';
import dayjs from 'dayjs';

import { catalogApi, listAll, policiesApi } from '#/api/backend';
import StatusTag from '#/components/status-tag.vue';
import { errorToast, useAction } from '#/composables/api';
import { toShanghaiISO } from '#/utils/format';

import { diffKeys, fieldOf, rawValue, versionLabel } from './fields';

const props = defineProps<{
  boundId?: null | string;
  draft: PolicyVersion;
  ifMatch: null | string;
  kind: PolicyKind;
  versions: PolicyVersion[];
}>();
const emit = defineEmits<{ conflict: []; published: [] }>();
const open = defineModel<boolean>('open', { default: false });

const effectiveAt = ref<Dayjs>(dayjs());
const plans = ref<Plan[]>([]);
const plansLoading = ref(false);
const compareId = ref<string | undefined>();
const action = useAction();

/** 可作对比的 active 版本；GPU 定价只与同型号、同使用方式的比较。 */
const candidates = computed(() =>
  props.versions.filter(
    (v) =>
      v.status === 'active' &&
      v.id !== props.draft.id &&
      (props.kind !== 'gpu-pricing' ||
        (v.config.gpu_model === props.draft.config.gpu_model && v.config.access_mode === props.draft.config.access_mode)),
  ),
);

function defaultCompare(): string | undefined {
  const list = candidates.value;
  const bound = list.find((v) => v.id === props.boundId);
  if (bound) return bound.id;
  const now = Date.now();
  const time = (v: PolicyVersion) => (v.effective_at ? Date.parse(v.effective_at) : 0);
  const effective = list.filter((v) => time(v) <= now);
  const pool = effective.length > 0 ? effective : list;
  return pool.reduce<PolicyVersion | undefined>((best, v) => (!best || time(v) > time(best) ? v : best), undefined)?.id;
}

const current = computed(() => candidates.value.find((v) => v.id === compareId.value) ?? null);

const diffRows = computed(() => {
  const oldConfig = current.value?.config ?? {};
  const newConfig = props.draft.config;
  return diffKeys(props.kind, oldConfig, newConfig).map((key) => {
    const before = current.value ? rawValue(oldConfig[key]) : '—';
    const after = rawValue(newConfig[key]);
    return { after, before, changed: before !== after, key, label: fieldOf(props.kind, key)?.label ?? key };
  });
});

const referencingPlans = computed(() =>
  compareId.value ? plans.value.filter((p) => p.policy_versions[props.kind] === compareId.value) : [],
);

watch(open, async (v) => {
  if (!v) return;
  effectiveAt.value = dayjs();
  compareId.value = defaultCompare();
  action.resetKey();
  plansLoading.value = true;
  try {
    plans.value = await listAll(catalogApi.plans);
  } catch (error) {
    plans.value = [];
    errorToast(error);
  } finally {
    plansLoading.value = false;
  }
});

watch(effectiveAt, () => action.resetKey());

async function publish() {
  if (!effectiveAt.value) return;
  const at = toShanghaiISO(effectiveAt.value.toDate());
  const r = await action.run(
    (key) => policiesApi.publish(props.kind, props.draft.id, at, { ifMatch: props.ifMatch, key }),
    { onConflict: () => emit('conflict'), success: '已发布' },
  );
  if (!r) return;
  open.value = false;
  emit('published');
}
</script>

<template>
  <Modal
    v-model:open="open"
    :confirm-loading="action.pending.value"
    destroy-on-close
    :ok-button-props="{ disabled: !effectiveAt }"
    ok-text="发布"
    :title="`发布 ${versionLabel(draft)}`"
    :width="760"
    @ok="publish"
  >
    <div class="max-h-[65vh] overflow-y-auto pr-2">
      <div class="mb-3 flex items-center gap-2">
        <span class="text-sm">生效时间</span>
        <DatePicker v-model:value="effectiveAt" :allow-clear="false" show-time />
        <span class="text-muted-foreground text-xs">按 +08:00 提交</span>
      </div>

      <div class="mb-2 flex items-center gap-2">
        <span class="text-sm">对比当前生效版本</span>
        <Select
          v-model:value="compareId"
          allow-clear
          class="w-80"
          :options="candidates.map((v) => ({ label: `${versionLabel(v)}${v.id === boundId ? '（平台默认）' : ''}`, value: v.id }))"
          placeholder="没有可对比的生效版本"
        />
      </div>
      <Table
        :columns="[
          { dataIndex: 'label', title: '字段' },
          { key: 'before', title: '当前' },
          { key: 'after', title: '新版本' },
        ]"
        :data-source="diffRows"
        :pagination="false"
        row-key="key"
        size="small"
      >
        <template #bodyCell="{ column, record }">
          <template v-if="column.dataIndex === 'label'">
            {{ record.label }} <span class="text-muted-foreground font-mono text-xs">{{ record.key }}</span>
          </template>
          <span v-else-if="column.key === 'before'" class="font-mono">{{ record.before }}</span>
          <span v-else-if="column.key === 'after'" class="font-mono" :class="record.changed ? 'font-semibold text-orange-500' : ''">
            {{ record.after }}
          </span>
        </template>
      </Table>

      <h4 class="mb-1 mt-4 font-medium">引用当前生效版本的套餐</h4>
      <div class="text-muted-foreground mb-2 text-xs">这些套餐需要发布新版本才会使用新政策</div>
      <Table
        v-if="plansLoading || referencingPlans.length > 0"
        :columns="[
          { dataIndex: 'id', title: 'ID', width: 80 },
          { dataIndex: 'name', title: '名称' },
          { key: 'code', title: '编号' },
          { key: 'status', title: '状态', width: 100 },
        ]"
        :data-source="referencingPlans"
        :loading="plansLoading"
        :pagination="false"
        row-key="id"
        size="small"
      >
        <template #bodyCell="{ column, record }">
          <span v-if="column.dataIndex === 'id'" class="font-mono">{{ record.id }}</span>
          <span v-else-if="column.key === 'code'" class="font-mono">{{ record.code }} v{{ record.version }}</span>
          <StatusTag v-else-if="column.key === 'status'" kind="catalogStatus" :value="record.status" />
        </template>
      </Table>
      <Empty v-else :image="Empty.PRESENTED_IMAGE_SIMPLE" description="没有套餐引用当前生效版本" />

      <Alert class="mt-3" message="已开始的计费区间和停机期限不受影响；发布后不能修改，只能退役" show-icon type="warning" />
    </div>
  </Modal>
</template>
