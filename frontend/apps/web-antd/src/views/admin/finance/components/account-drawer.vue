<script lang="ts" setup>
import type { Dayjs } from 'dayjs';

import type { PolicyVersion } from '#/api/types';

import { computed, ref, watch } from 'vue';

import { Button, DatePicker, Descriptions, DescriptionsItem, Drawer, Input, message, Select, Space } from 'ant-design-vue';

import { financeApi } from '#/api/backend';
import ConfirmAction from '#/components/confirm-action.vue';
import StatusTag from '#/components/status-tag.vue';
import TimeText from '#/components/time-text.vue';
import UserName from '#/components/user-name.vue';
import { errorToast, useAction, useResource } from '#/composables/api';
import { formatPt, ptToMicros, SIGNED_PT_PATTERN, toShanghaiISO } from '#/utils/format';

import { isEffective, policyLabel } from './credit-policy';

const props = defineProps<{ versions: PolicyVersion[] }>();
const emit = defineEmits<{ changed: [] }>();
const accountId = defineModel<null | string>('id', { default: null });

const detail = useResource(() => (accountId.value ? financeApi.account(accountId.value) : null));
const open = computed({
  get: () => !!accountId.value,
  set: (v) => {
    if (!v) accountId.value = null;
  },
});

async function load() {
  try {
    await detail.reload();
  } catch (error) {
    errorToast(error);
  }
}

watch(accountId, (id) => {
  detail.data.value = null;
  detail.etag.value = null;
  if (id) void load();
});

async function refresh() {
  await load();
  emit('changed');
}

/* 切换额度政策：PATCH {policy_version_id}，If-Match 账户 ETag。 */
const switchOpen = ref(false);
const switchTarget = ref<string | undefined>();
const switchAction = useAction();
const policyOptions = computed(() =>
  props.versions.filter((v) => isEffective(v)).map((v) => ({ label: `${v.code} v${v.version}`, value: v.id })),
);
watch(switchTarget, () => switchAction.resetKey());

function openSwitch() {
  switchTarget.value = undefined;
  switchOpen.value = true;
}

async function submitSwitch() {
  const policyId = switchTarget.value;
  if (!policyId) {
    message.warning('请选择额度政策版本');
    return false;
  }
  const id = accountId.value;
  if (!id) return false;
  const r = await switchAction.run(
    () => financeApi.switchPolicy(id, { policy_version_id: policyId }, { ifMatch: detail.etag.value }),
    { onConflict: load, success: '已切换额度政策' },
  );
  if (!r) return false;
  await refresh();
  return true;
}

/* 调整额度：POST {amount_pt, expires_at|null} → 201，If-Match 账户 ETag。 */
const adjustOpen = ref(false);
const adjustAmount = ref('');
const adjustExpires = ref<Dayjs | undefined>();
const adjustAction = useAction();
watch([adjustAmount, adjustExpires], () => adjustAction.resetKey());

function openAdjust() {
  adjustAmount.value = '';
  adjustExpires.value = undefined;
  adjustOpen.value = true;
}

async function submitAdjust() {
  const amount = adjustAmount.value.trim();
  if (!SIGNED_PT_PATTERN.test(amount) || !ptToMicros(amount)) {
    message.warning('请输入非零金额，最多 6 位小数');
    return false;
  }
  const expires = adjustExpires.value;
  if (expires && !expires.isAfter(Date.now())) {
    message.warning('过期时间必须晚于现在');
    return false;
  }
  const id = accountId.value;
  if (!id) return false;
  const body = { amount_pt: amount, expires_at: expires ? toShanghaiISO(expires.toDate()) : null };
  const r = await adjustAction.run(
    (key) => financeApi.adjust(id, body, { ifMatch: detail.etag.value, key }),
    { onConflict: load, success: '已调整额度' },
  );
  if (!r) return false;
  await refresh();
  return true;
}
</script>

<template>
  <Drawer v-model:open="open" :title="`额度账户 #${accountId ?? ''}`" :width="560">
    <template v-if="detail.data.value" #extra>
      <Space>
        <Button @click="openSwitch">切换额度政策</Button>
        <Button type="primary" @click="openAdjust">调整额度</Button>
      </Space>
    </template>
    <Descriptions v-if="detail.data.value" bordered :column="1" size="small">
      <DescriptionsItem label="用户"><UserName :id="detail.data.value.user_id" link /></DescriptionsItem>
      <DescriptionsItem label="状态"><StatusTag kind="accountStatus" :value="detail.data.value.status" /></DescriptionsItem>
      <DescriptionsItem label="剩余">
        <span class="font-mono">{{ formatPt(detail.data.value.remaining_pt) }} pt</span>
      </DescriptionsItem>
      <DescriptionsItem label="已预留">
        <span class="font-mono">{{ formatPt(detail.data.value.reserved_pt) }} pt</span>
      </DescriptionsItem>
      <DescriptionsItem label="可用">
        <span class="font-mono">{{ formatPt(detail.data.value.available_pt) }} pt</span>
      </DescriptionsItem>
      <DescriptionsItem label="额度政策">
        <span class="font-mono">{{ policyLabel(versions, detail.data.value.policy_version_id) }}</span>
      </DescriptionsItem>
      <DescriptionsItem label="下次发放"><TimeText :value="detail.data.value.next_grant_at" /></DescriptionsItem>
      <DescriptionsItem label="额度事件">
        <Space v-if="detail.data.value.enforcement">
          <span class="font-mono">#{{ detail.data.value.enforcement.id }}</span>
          <StatusTag kind="enforcement" :value="detail.data.value.enforcement.status" />
          <span>停机期限</span>
          <TimeText :value="detail.data.value.enforcement.shutdown_deadline" />
        </Space>
        <span v-else>—</span>
      </DescriptionsItem>
      <DescriptionsItem label="revision"><span class="font-mono">{{ detail.data.value.revision }}</span></DescriptionsItem>
    </Descriptions>

    <ConfirmAction
      v-model:open="switchOpen"
      description="账户有活动计费区间或未完成的停机事件时，后端会拒绝切换。"
      :on-confirm="submitSwitch"
      title="切换额度政策"
    >
      <div class="mb-3">
        <div class="mb-1 text-sm">额度政策版本（仅已生效的 active 版本）</div>
        <Select
          v-model:value="switchTarget"
          class="w-full"
          :options="policyOptions"
          placeholder="选择版本"
          show-search
          option-filter-prop="label"
        />
      </div>
    </ConfirmAction>

    <ConfirmAction
      v-model:open="adjustOpen"
      description="正数增加额度，负数扣减；扣减不能低于已预留部分。"
      :on-confirm="submitAdjust"
      title="调整额度"
    >
      <div class="mb-3">
        <div class="mb-1 text-sm">金额（pt，可为负，最多 6 位小数）</div>
        <Input v-model:value="adjustAmount" class="font-mono" placeholder="例如 100 或 -20.5" suffix="pt" />
      </div>
      <div class="mb-3">
        <div class="mb-1 text-sm">过期时间（可选，不填则不过期）</div>
        <DatePicker v-model:value="adjustExpires" class="w-full" show-time />
      </div>
    </ConfirmAction>
  </Drawer>
</template>
