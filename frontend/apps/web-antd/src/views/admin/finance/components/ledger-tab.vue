<script lang="ts" setup>
import type { Dayjs } from 'dayjs';

import type { Query } from '#/api/backend';
import type { LedgerEntry } from '#/api/types';

import { onMounted, ref, watch } from 'vue';

import { DatePicker, Input, Table } from 'ant-design-vue';

import { financeApi } from '#/api/backend';
import CursorPager from '#/components/cursor-pager.vue';
import StatusTag from '#/components/status-tag.vue';
import TimeText from '#/components/time-text.vue';
import { useCursorList } from '#/composables/api';
import { formatPt, toShanghaiISO } from '#/utils/format';

const props = defineProps<{ instanceId?: string; userId?: string }>();

// 管理端流水只接受 user_id、instance_id、from、to（无 event_type）。
const user = ref(props.userId ?? '');
const instance = ref(props.instanceId ?? '');
const range = ref<[Dayjs, Dayjs] | undefined>();

function currentFilters(): Query {
  return {
    from: range.value ? toShanghaiISO(range.value[0].toDate()) : undefined,
    instance_id: instance.value || undefined,
    to: range.value ? toShanghaiISO(range.value[1].toDate()) : undefined,
    user_id: user.value || undefined,
  };
}

const filters = ref<Query>(currentFilters());
watch([user, instance, range], () => {
  filters.value = currentFilters();
});

const list = useCursorList<LedgerEntry>(financeApi.ledger, filters);

/** 带符号显示：正数加 "+"。 */
function signed(value: null | string): string {
  const v = formatPt(value);
  return /^\d/.test(v) && v !== '0' ? `+${v}` : v;
}

const columns = [
  { key: 'time', title: '时间', width: 130 },
  { key: 'type', title: '类型', width: 100 },
  { key: 'amount', title: '额度变动', align: 'right' as const },
  { key: 'reserved', title: '预留变动', align: 'right' as const },
  { key: 'remaining_after', title: '变动后剩余', align: 'right' as const },
  { key: 'reserved_after', title: '变动后预留', align: 'right' as const },
];

onMounted(() => list.load());
</script>

<template>
  <div>
    <div class="mb-3 flex flex-wrap items-center gap-2">
      <Input v-model:value="user" allow-clear class="!w-36" placeholder="用户 ID" />
      <Input v-model:value="instance" allow-clear class="!w-36" placeholder="实例 ID" />
      <DatePicker.RangePicker v-model:value="range" class="max-w-full" show-time />
    </div>
    <Table
      :columns="columns"
      :data-source="list.items.value"
      :loading="list.loading.value"
      :pagination="false"
      :scroll="{ x: 'max-content' }"
      row-key="id"
      size="small"
    >
      <template #bodyCell="{ column, record }">
        <template v-if="column.key === 'time'"><TimeText :value="record.created_at" /></template>
        <template v-else-if="column.key === 'type'"><StatusTag kind="entryType" :value="record.entry_type" /></template>
        <template v-else-if="column.key === 'amount'">
          <span class="font-mono">{{ signed(record.amount_delta_pt) }}</span>
        </template>
        <template v-else-if="column.key === 'reserved'">
          <span class="font-mono">{{ signed(record.reserved_delta_pt) }}</span>
        </template>
        <template v-else-if="column.key === 'remaining_after'">
          <span class="font-mono">{{ formatPt(record.remaining_after_pt) }}</span>
        </template>
        <template v-else-if="column.key === 'reserved_after'">
          <span class="font-mono">{{ formatPt(record.reserved_after_pt) }}</span>
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
  </div>
</template>
