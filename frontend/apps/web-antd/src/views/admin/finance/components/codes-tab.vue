<script lang="ts" setup>
import type { ID, RedemptionCode } from '#/api/types';

import { onMounted, ref, shallowRef } from 'vue';

import { Alert, Button, Select, Space, Table } from 'ant-design-vue';

import { financeApi } from '#/api/backend';
import { revisionTag } from '#/api/request';
import ConfirmAction from '#/components/confirm-action.vue';
import CursorPager from '#/components/cursor-pager.vue';
import StatusTag from '#/components/status-tag.vue';
import TimeText from '#/components/time-text.vue';
import { useAction, useCursorList } from '#/composables/api';
import { ENUMS } from '#/locales/enums';
import { formatPt } from '#/utils/format';

import GenerateCodesModal from './generate-codes-modal.vue';
import RevealCodesModal from './reveal-codes-modal.vue';

const filters = ref<{ status: string | undefined }>({ status: undefined });
const list = useCursorList<RedemptionCode>(financeApi.codes, filters);
const statusOptions = Object.entries(ENUMS.codeStatus ?? {}).map(([value, l]) => ({ label: l.text, value }));

const columns = [
  { dataIndex: 'id', title: 'ID', width: 80 },
  { key: 'prefix', title: '前缀' },
  { key: 'grant', title: '面额', align: 'right' as const },
  { key: 'uses', title: '已用 / 上限', width: 110 },
  { key: 'valid_from', title: '生效', width: 130 },
  { key: 'expires', title: '过期', width: 130 },
  { key: 'status', title: '状态', width: 90 },
  { key: 'actions', title: '操作', width: 80 },
];

const generateOpen = ref(false);
// 明文兑换码只保存在这里，弹窗关闭时清空。
const revealed = shallowRef<RedemptionCode[]>([]);
const replayedIds = ref<ID[] | null>(null);

function onCreated(items: RedemptionCode[]) {
  replayedIds.value = null;
  revealed.value = items;
  void list.load();
}

function onReplayed(ids: ID[]) {
  replayedIds.value = ids;
  void list.load();
}

const disableTarget = ref<null | RedemptionCode>(null);
const disableOpen = ref(false);
const disableAction = useAction();

function openDisable(code: RedemptionCode) {
  disableTarget.value = code;
  disableOpen.value = true;
}

async function submitDisable() {
  const target = disableTarget.value;
  if (!target) return false;
  const r = await disableAction.run(
    (key) => financeApi.disableCode(target.id, { ifMatch: revisionTag(target.revision), key }),
    { onConflict: () => list.load(), success: '已禁用兑换码' },
  );
  if (!r) return false;
  await list.load();
  return true;
}

onMounted(() => list.load());
</script>

<template>
  <div>
    <Alert
      v-if="replayedIds"
      class="mb-3"
      closable
      show-icon
      type="warning"
      @close="replayedIds = null"
    >
      <template #message>
        上次生成结果已不可查看，请禁用该批次后重新生成
        <span v-if="replayedIds.length" class="font-mono">（记录 ID：{{ replayedIds.join(', ') }}）</span>
      </template>
    </Alert>
    <div class="mb-3 flex flex-wrap items-center justify-between gap-2">
      <Space wrap>
        <Select
          v-model:value="filters.status"
          allow-clear
          class="w-32"
          :options="statusOptions"
          placeholder="状态"
        />
      </Space>
      <Button type="primary" @click="generateOpen = true">生成兑换码</Button>
    </div>
    <Table
      :columns="columns"
      :data-source="list.items.value"
      :loading="list.loading.value"
      :pagination="false"
      row-key="id"
      size="small"
    >
      <template #bodyCell="{ column, record }">
        <template v-if="column.key === 'prefix'"><span class="font-mono">{{ record.display_prefix }}…</span></template>
        <template v-else-if="column.key === 'grant'"><span class="font-mono">{{ formatPt(record.grant_pt) }}</span></template>
        <template v-else-if="column.key === 'uses'">
          <span class="font-mono">{{ record.used_count }} / {{ record.max_uses }}</span>
        </template>
        <template v-else-if="column.key === 'valid_from'"><TimeText :value="record.valid_from" /></template>
        <template v-else-if="column.key === 'expires'"><TimeText :value="record.expires_at" /></template>
        <template v-else-if="column.key === 'status'"><StatusTag kind="codeStatus" :value="record.status" /></template>
        <template v-else-if="column.key === 'actions'">
          <Button
            v-if="record.status === 'active'"
            danger
            size="small"
            type="link"
            @click="openDisable(record as RedemptionCode)"
          >
            禁用
          </Button>
        </template>
      </template>
    </Table>
    <CursorPager
      :has-next="list.hasNext()"
      :has-prev="list.hasPrev()"
      :loading="list.loading.value"
      :page="list.page()"
      @next="list.nextPage"
      @prev="list.prevPage"
      @refresh="list.load"
    />

    <GenerateCodesModal v-model:open="generateOpen" @created="onCreated" @replayed="onReplayed" />
    <RevealCodesModal v-model:codes="revealed" />
    <ConfirmAction
      v-model:open="disableOpen"
      danger
      :description="`禁用后兑换码 ${disableTarget?.display_prefix ?? ''}… 不能再兑换，已兑换的额度不受影响。`"
      ok-text="禁用"
      :on-confirm="submitDisable"
      title="禁用兑换码"
    />
  </div>
</template>
