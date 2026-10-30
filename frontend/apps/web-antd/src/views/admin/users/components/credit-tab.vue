<script lang="ts" setup>
import type { CreditAccount } from '#/api/types';

import { onMounted, ref, shallowRef } from 'vue';

import { Alert, Button, Descriptions, DescriptionsItem, Skeleton, Space } from 'ant-design-vue';

import { financeApi } from '#/api/backend';
import StatusTag from '#/components/status-tag.vue';
import TimeText from '#/components/time-text.vue';
import { errorToast } from '#/composables/api';
import { formatPt } from '#/utils/format';

const props = defineProps<{ userId: string }>();

const account = shallowRef<CreditAccount | null>(null);
const loaded = ref(false);
const loading = ref(true);

async function load() {
  loading.value = true;
  try {
    const r = await financeApi.accounts({ limit: 1, user_id: props.userId });
    account.value = r.data.items[0] ?? null;
    loaded.value = true;
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
    <Alert
      v-else-if="loaded && !account"
      description="额度账户由后台每分钟的发放任务自动创建，新用户约 1 分钟后可见。"
      message="额度账户初始化中"
      show-icon
      type="info"
    >
      <template #action><Button :loading="loading" size="small" @click="load">刷新</Button></template>
    </Alert>
    <template v-else-if="account">
      <Descriptions bordered :column="1" size="small">
        <DescriptionsItem label="账户 ID"><span class="font-mono">{{ account.id }}</span></DescriptionsItem>
        <DescriptionsItem label="状态"><StatusTag kind="accountStatus" :value="account.status" /></DescriptionsItem>
        <DescriptionsItem label="可用额度">
          <span class="font-mono">{{ formatPt(account.available_pt) }} pt</span>
        </DescriptionsItem>
        <DescriptionsItem label="剩余额度">
          <span class="font-mono">{{ formatPt(account.remaining_pt) }} pt</span>
        </DescriptionsItem>
        <DescriptionsItem label="已预留">
          <span class="font-mono">{{ formatPt(account.reserved_pt) }} pt</span>
        </DescriptionsItem>
        <DescriptionsItem label="额度政策版本">
          <span class="font-mono">#{{ account.policy_version_id }}</span>
        </DescriptionsItem>
        <DescriptionsItem label="下次发放"><TimeText :value="account.next_grant_at" /></DescriptionsItem>
        <DescriptionsItem label="额度事件">
          <span v-if="!account.enforcement" class="text-muted-foreground">无</span>
          <Space v-else>
            <StatusTag kind="enforcement" :value="account.enforcement.status" />
            <span class="font-mono text-xs">#{{ account.enforcement.id }}</span>
            <span>停机期限 <TimeText :value="account.enforcement.shutdown_deadline" /></span>
          </Space>
        </DescriptionsItem>
      </Descriptions>
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
