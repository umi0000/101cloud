<script lang="ts" setup>
import type { Dayjs } from 'dayjs';

import type { ID, RedemptionCode } from '#/api/types';

import { computed, ref, watch } from 'vue';

import { DatePicker, Input, InputNumber, message, Modal } from 'ant-design-vue';

import { financeApi } from '#/api/backend';
import { useAction } from '#/composables/api';
import { PT_PATTERN, ptToMicros, toShanghaiISO } from '#/utils/format';

const emit = defineEmits<{
  /** 首次生成成功：含明文 code，只交给调用方展示一次。 */
  created: [items: RedemptionCode[]];
  /** 同一幂等键重放：不含明文，只有记录 id。 */
  replayed: [ids: ID[]];
}>();
const open = defineModel<boolean>('open', { default: false });

// 后端限制：amount_pt > 0；expires_at 晚于现在；max_redemptions 为 uint32 且 ≥ 1；count 1–1000。
const MAX_UINT32 = 4_294_967_295;
const amount = ref('');
const expires = ref<Dayjs | undefined>();
const maxRedemptions = ref<number | undefined>(1);
const count = ref<number | undefined>(1);
const action = useAction();

watch(open, (v) => {
  if (!v) return;
  amount.value = '';
  expires.value = undefined;
  maxRedemptions.value = 1;
  count.value = 1;
  action.resetKey();
});
watch([amount, expires, maxRedemptions, count], () => action.resetKey());

const valid = computed(() => {
  const a = amount.value.trim();
  return (
    PT_PATTERN.test(a) &&
    !!ptToMicros(a) &&
    !!expires.value &&
    !!maxRedemptions.value &&
    maxRedemptions.value >= 1 &&
    !!count.value &&
    count.value >= 1 &&
    count.value <= 1000
  );
});

async function submit() {
  if (!valid.value || !expires.value || !maxRedemptions.value || !count.value) return;
  if (!expires.value.isAfter(Date.now())) {
    message.warning('过期时间必须晚于现在');
    return;
  }
  const body = {
    amount_pt: amount.value.trim(),
    count: count.value,
    expires_at: toShanghaiISO(expires.value.toDate()),
    max_redemptions: maxRedemptions.value,
  };
  const r = await action.run((key) => financeApi.createCodes(body, { key }));
  if (!r) return;
  open.value = false;
  if (r.data.codes_revealable && r.data.items) emit('created', r.data.items);
  else emit('replayed', r.data.record_ids ?? []);
}
</script>

<template>
  <Modal
    v-model:open="open"
    :confirm-loading="action.pending.value"
    destroy-on-close
    :ok-button-props="{ disabled: !valid }"
    ok-text="生成"
    title="生成兑换码"
    @ok="submit"
  >
    <div class="mb-3">
      <div class="mb-1 text-sm">面额（pt，大于 0，最多 6 位小数）</div>
      <Input v-model:value="amount" class="font-mono" placeholder="例如 100" suffix="pt" />
    </div>
    <div class="mb-3">
      <div class="mb-1 text-sm">过期时间</div>
      <DatePicker v-model:value="expires" class="w-full" show-time />
    </div>
    <div class="mb-3">
      <div class="mb-1 text-sm">每个兑换码可用次数</div>
      <InputNumber v-model:value="maxRedemptions" class="w-full" :max="MAX_UINT32" :min="1" :precision="0" />
    </div>
    <div class="mb-3">
      <div class="mb-1 text-sm">生成数量（1–1000）</div>
      <InputNumber v-model:value="count" class="w-full" :max="1000" :min="1" :precision="0" />
    </div>
  </Modal>
</template>
