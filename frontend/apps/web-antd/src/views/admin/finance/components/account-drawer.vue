<script lang="ts" setup>
import type { Dayjs } from 'dayjs';

import type { CreditAdjustmentResult, PolicyVersion } from '#/api/types';

import { computed, ref, watch } from 'vue';

import { Alert, Button, DatePicker, Descriptions, DescriptionsItem, Drawer, Input, message, Select, Space } from 'ant-design-vue';

import { financeApi } from '#/api/backend';
import { revisionTag } from '#/api/request';
import ConfirmAction from '#/components/confirm-action.vue';
import StatusTag from '#/components/status-tag.vue';
import TimeText from '#/components/time-text.vue';
import UserName from '#/components/user-name.vue';
import { errorToast, useAction, usePolling, useResource } from '#/composables/api';
import { formatPt, ptToMicros, SIGNED_PT_PATTERN, toShanghaiISO } from '#/utils/format';

import { isEffective, policyLabel } from './credit-policy';

const props = defineProps<{ versions: PolicyVersion[] }>();
const emit = defineEmits<{ changed: [] }>();
const accountId = defineModel<null | string>('id', { default: null });

const detail = useResource(() => (accountId.value ? financeApi.account(accountId.value) : null));
const settlementRefreshFailed = ref(false);
const open = computed({
  get: () => !!accountId.value,
  set: (v) => {
    if (!v) accountId.value = null;
  },
});

async function load() {
  const id = accountId.value;
  try {
    await detail.reload();
    if (id && accountId.value === id) {
      adjustmentRefreshFailed.value[id] = false;
      settlementRefreshFailed.value = false;
    }
  } catch (error) {
    errorToast(error);
  }
}

watch(accountId, (id) => {
  detail.data.value = null;
  detail.etag.value = null;
  settlementRefreshFailed.value = false;
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
const adjustmentResults = ref<Record<string, CreditAdjustmentResult>>({});
const adjustmentRefreshFailed = ref<Record<string, boolean>>({});
const lastAdjustment = computed(() => accountId.value ? adjustmentResults.value[accountId.value] : undefined);
watch([adjustAmount, adjustExpires], () => adjustAction.resetKey());

let settlementRefreshing = false;
usePolling(async () => {
  const id = accountId.value;
  if (!id || !detail.data.value?.settlement_pending || detail.loading.value || settlementRefreshing || adjustOpen.value || switchOpen.value) return;
  const etag = detail.etag.value;
  settlementRefreshing = true;
  try {
    const r = await financeApi.account(id);
    // A late poll must not overwrite another account or a newer mutation/refresh.
    if (accountId.value !== id || detail.etag.value !== etag || detail.loading.value) return;
    detail.data.value = r.data;
    detail.etag.value = r.etag;
    settlementRefreshFailed.value = false;
    adjustmentRefreshFailed.value[id] = false;
    if (!r.data.settlement_pending) emit('changed');
  } catch {
    if (accountId.value === id && detail.etag.value === etag) settlementRefreshFailed.value = true;
  } finally {
    settlementRefreshing = false;
  }
}, () => 2000);

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
    { onConflict: load },
  );
  if (!r) return false;
  adjustmentResults.value[id] = r.data;
  adjustmentRefreshFailed.value[id] = false;
  adjustOpen.value = false;
  if (accountId.value === id) {
    // The committed response is authoritative even if the subsequent GET fails.
    if (detail.data.value) {
      detail.data.value = {
        ...detail.data.value,
        available_pt: r.data.available_pt,
        outstanding_debt_pt: r.data.outstanding_debt_pt,
        remaining_pt: r.data.remaining_pt,
        reserved_pt: r.data.reserved_pt,
        revision: r.data.revision,
        settlement_pending: r.data.settlement_pending,
      };
    }
    detail.etag.value = revisionTag(r.data.revision);
    try {
      await detail.reload();
      settlementRefreshFailed.value = false;
    } catch {
      adjustmentRefreshFailed.value[id] = true;
    }
  }
  emit('changed');
  return true;
}
</script>

<template>
  <Drawer v-model:open="open" :title="`额度账户 #${accountId ?? ''}`" width="min(560px, 100vw)">
    <template v-if="detail.data.value" #extra>
      <Space>
        <Button @click="openSwitch">切换额度政策</Button>
        <Button type="primary" @click="openAdjust">调整额度</Button>
      </Space>
    </template>
    <Alert
      v-if="lastAdjustment"
      class="mb-4"
      show-icon
      type="success"
      :message="lastAdjustment.bucket_id ? '最近一次额度发放已入账' : '最近一次额度扣减已入账'"
      role="status"
    >
      <template #description>
        <Descriptions class="mt-2" :column="1" size="small">
          <DescriptionsItem :label="lastAdjustment.bucket_id ? '发放金额' : '扣减金额（带符号）'">
            <span class="font-mono">{{ lastAdjustment.bucket_id ? '+' : '' }}{{ formatPt(lastAdjustment.amount_pt) }} pt</span>
          </DescriptionsItem>
          <DescriptionsItem label="入账事务已偿还欠款"><span class="font-mono">{{ formatPt(lastAdjustment.settled_debt_pt) }} pt</span></DescriptionsItem>
          <DescriptionsItem label="入账后可用（净额）"><span class="font-mono">{{ formatPt(lastAdjustment.available_pt) }} pt</span></DescriptionsItem>
          <DescriptionsItem label="提交时未偿欠款（总额）"><span class="font-mono">{{ formatPt(lastAdjustment.outstanding_debt_pt) }} pt</span></DescriptionsItem>
          <DescriptionsItem label="提交时后台抵扣">{{ lastAdjustment.settlement_pending ? '当时已安排后台处理（非当前状态）' : '当时无待处理任务（不代表欠款已还清）' }}</DescriptionsItem>
        </Descriptions>
        <div v-if="lastAdjustment.settlement_pending">提交时已安排历史欠费抵扣；此处仅为入账快照，不代表现在仍在处理。当前进度和欠款以以下账户详情为准，请勿重复提交。</div>
        <div v-if="lastAdjustment.bucket_id">发放金额已反映在净余额中；欠款未还清时余额仍可能为负数，并非发放失败。可用净额已扣除欠款及预留，请勿再次扣除未偿欠款。</div>
        <div v-else>本次为负数扣减，不是额度发放。</div>
        <div>以上为最近一次调整提交成功时的快照，后续后台抵扣及计费可能改变欠款或余额。</div>
      </template>
    </Alert>
    <Alert
      v-if="accountId && adjustmentRefreshFailed[accountId]"
      class="mb-4"
      message="调整已入账，但账户详情刷新失败；请勿重复提交。上方结果仍为本次真实入账结果，列表刷新不影响该结果。"
      show-icon
      type="warning"
    />
    <Alert
      v-if="settlementRefreshFailed"
      class="mb-4"
      message="抵扣状态刷新失败，当前进度未知"
      description="以下保留上次查询结果，不代表仍在抵扣；页面将自动重试，也可手动刷新。额度已入账，请勿重复发放。"
      show-icon
      type="warning"
    />
    <Alert
      v-if="detail.data.value?.settlement_pending && !settlementRefreshFailed"
      class="mb-4"
      description="历史欠费正在后台分批抵扣，每2秒自动刷新进度，完成后此提示自动消失。剩余与可用已按欠款计算净额，请勿再次扣除欠款或重复发放。"
      message="账户有待处理的后台欠费抵扣"
      role="status"
      show-icon
      type="info"
    />
    <Descriptions v-if="detail.data.value" bordered :column="1" size="small">
      <DescriptionsItem label="用户"><UserName :id="detail.data.value.user_id" link /></DescriptionsItem>
      <DescriptionsItem label="状态"><StatusTag kind="accountStatus" :value="detail.data.value.status" /></DescriptionsItem>
      <DescriptionsItem label="剩余（净额）">
        <span class="font-mono">{{ formatPt(detail.data.value.remaining_pt) }} pt</span>
      </DescriptionsItem>
      <DescriptionsItem label="未偿欠款（总额）">
        <span class="font-mono">{{ formatPt(detail.data.value.outstanding_debt_pt) }} pt</span>
      </DescriptionsItem>
      <DescriptionsItem label="已预留">
        <span class="font-mono">{{ formatPt(detail.data.value.reserved_pt) }} pt</span>
      </DescriptionsItem>
      <DescriptionsItem label="可用（净额）">
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
    <p v-if="detail.data.value" class="text-muted-foreground mt-3 text-xs">剩余与可用已扣除欠款，可用还扣除预留；欠款未还清时可为负数。未偿欠款为后台已记录的总额，不能从净余额中再次扣除。无待处理任务不代表欠款已还清。</p>
    <Button v-if="detail.data.value || lastAdjustment" :loading="detail.loading.value" @click="refresh">刷新账户详情</Button>

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
      description="正数发放提交成功即入账，历史欠费由后台分批抵扣，不等待全部偿债完成；未偿欠款总额可能暂未下降。净余额会立即反映发放金额，欠款未还清时仍可能为负数，并非发放失败；可用已扣除欠款及预留。负数为扣减，不是发放，且不能动用欠款及预留所需额度。"
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
