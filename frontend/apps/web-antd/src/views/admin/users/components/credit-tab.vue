<script lang="ts" setup>
import type { CreditAccount, PolicyVersion } from '#/api/types';

import { computed, onMounted, ref, shallowRef, watch } from 'vue';

import { Alert, Button, Descriptions, DescriptionsItem, Form, FormItem, Select, Skeleton, Space } from 'ant-design-vue';

import { financeApi, listAll, policiesApi } from '#/api/backend';
import StatusTag from '#/components/status-tag.vue';
import TimeText from '#/components/time-text.vue';
import { errorToast, useAction } from '#/composables/api';
import { formatPt } from '#/utils/format';

import { isEffective } from '../../finance/components/credit-policy';

const props = defineProps<{ userId: string }>();

const account = shallowRef<CreditAccount | null>(null);
const loaded = ref(false);
const loading = ref(true);
const creditVersions = ref<PolicyVersion[]>([]);
const policiesLoading = ref(false);
const policiesFailed = ref(false);
const policyId = ref<string | undefined>();
const setup = useAction();
const policyOptions = computed(() =>
  creditVersions.value.filter((v) => isEffective(v)).map((v) => ({ label: `${v.code} v${v.version}`, value: v.id })),
);
const canSetup = computed(() =>
  !policiesLoading.value && !policiesFailed.value && policyOptions.value.some((p) => p.value === policyId.value),
);
watch(policyId, () => setup.resetKey());

async function loadPolicies() {
  policiesLoading.value = true;
  policiesFailed.value = false;
  try {
    creditVersions.value = await listAll((q) => policiesApi.list('credit', q));
  } catch (error) {
    policiesFailed.value = true;
    errorToast(error);
  } finally {
    policiesLoading.value = false;
  }
}

async function submitSetup() {
  const selectedPolicy = policyId.value;
  if (!selectedPolicy || !canSetup.value) return;
  const r = await setup.run(
    (key) => financeApi.createAccount({ user_id: props.userId, policy_version_id: selectedPolicy }, { key }),
    { success: '共享钱包已配置' },
  );
  if (r) account.value = r.data;
}

async function load() {
  loading.value = true;
  try {
    const r = await financeApi.accounts({ limit: 1, user_id: props.userId });
    account.value = r.data.items[0] ?? null;
    loaded.value = true;
    if (!account.value) await loadPolicies();
  } catch (error) {
    errorToast(error);
  } finally {
    loading.value = false;
  }
}

onMounted(load);
</script>

<template>
  <div class="max-w-2xl">
    <Skeleton v-if="!loaded && loading" active />
    <template v-else-if="loaded && !account">
      <Alert
        class="mb-3"
        description="请选择已生效的额度政策并创建共享钱包。创建账户本身不发放额度；后续每日额度按所选政策执行，选择实例套餐不会发放额度或切换钱包政策。"
        message="该用户尚未配置共享钱包"
        show-icon
        type="info"
      />
      <Form layout="vertical" @finish="submitSetup">
        <FormItem label="共享钱包额度政策" required>
          <Select
            v-model:value="policyId"
            class="w-full"
            :disabled="setup.pending.value || policiesLoading || policiesFailed"
            :loading="policiesLoading"
            option-filter-prop="label"
            :options="policyOptions"
            placeholder="请选择已生效的额度政策"
            show-search
          />
        </FormItem>
        <Alert v-if="policiesFailed" class="mb-3" message="额度政策加载失败" show-icon type="error">
          <template #action><Button size="small" @click="loadPolicies">重试</Button></template>
        </Alert>
        <Alert
          v-else-if="!policiesLoading && !policyOptions.length"
          class="mb-3"
          message="暂无已生效的额度政策，请先在政策配置中发布，再刷新选项。"
          show-icon
          type="warning"
        >
          <template #action><Button size="small" @click="loadPolicies">刷新</Button></template>
        </Alert>
        <Space>
          <Button :disabled="!canSetup" html-type="submit" :loading="setup.pending.value" type="primary">
            创建共享钱包
          </Button>
          <Button :disabled="setup.pending.value" :loading="loading" @click="load">刷新</Button>
        </Space>
      </Form>
    </template>
    <template v-else-if="account">
      <Alert
        v-if="account.settlement_pending"
        class="mb-3"
        description="额度已入账，历史欠费正在后台分批抵扣，未偿欠款总额可能暂未下降。净余额已反映入账金额，请刷新查看最新进展，不要重复发放。"
        message="后台欠费抵扣处理中"
        role="status"
        show-icon
        type="info"
      />
      <Descriptions bordered :column="1" size="small">
        <DescriptionsItem label="账户 ID"><span class="font-mono">{{ account.id }}</span></DescriptionsItem>
        <DescriptionsItem label="状态"><StatusTag kind="accountStatus" :value="account.status" /></DescriptionsItem>
        <DescriptionsItem label="可用额度（净额）">
          <span class="font-mono">{{ formatPt(account.available_pt) }} pt</span>
        </DescriptionsItem>
        <DescriptionsItem label="剩余额度（净额）">
          <span class="font-mono">{{ formatPt(account.remaining_pt) }} pt</span>
        </DescriptionsItem>
        <DescriptionsItem label="已预留">
          <span class="font-mono">{{ formatPt(account.reserved_pt) }} pt</span>
        </DescriptionsItem>
        <DescriptionsItem label="未偿欠款（总额）">
          <span class="font-mono">{{ formatPt(account.outstanding_debt_pt) }} pt</span>
        </DescriptionsItem>
        <DescriptionsItem label="额度政策版本">
          <span class="font-mono">#{{ account.policy_version_id }}</span>
        </DescriptionsItem>
        <DescriptionsItem label="下次发放"><TimeText :value="account.next_grant_at" /></DescriptionsItem>
        <DescriptionsItem label="额度事件">
          <span v-if="!account.enforcements.length" class="text-muted-foreground">无</span>
          <Space v-else direction="vertical">
            <Space v-for="event in account.enforcements" :key="event.id" wrap>
              <StatusTag kind="enforcement" :value="event.status" />
              <span class="font-mono text-xs">#{{ event.id }}</span>
              <span>政策 #{{ event.policy_version_id }}</span>
              <span>停机期限 <TimeText :value="event.shutdown_deadline" /></span>
            </Space>
          </Space>
        </DescriptionsItem>
      </Descriptions>
      <p class="text-muted-foreground mt-3 text-xs">这是用户共享钱包，独立于实例套餐。套餐的额度政策仅控制实例的宽限、停机与磁盘规则，不会重复发放每日额度或更改钱包政策。</p>
      <p class="text-muted-foreground mt-3 text-xs">剩余与可用已扣除欠款，可用还扣除预留；欠款未还清时可能为负数。未偿欠款总额不应从净余额中再次扣除，无待处理任务不代表欠款已还清。</p>
      <Space class="mt-3">
        <Button :loading="loading" @click="load">刷新</Button>
        <RouterLink to="/admin/finance">到“财务与额度”调整额度或切换政策</RouterLink>
      </Space>
    </template>
    <Alert v-else message="额度账户加载失败" show-icon type="error">
      <template #action><Button size="small" @click="load">重试</Button></template>
    </Alert>
  </div>
</template>
