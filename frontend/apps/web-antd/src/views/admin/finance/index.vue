<script lang="ts" setup>
import type { PolicyVersion } from '#/api/types';

import { onMounted, ref, shallowRef } from 'vue';
import { useRoute } from 'vue-router';

import { Page } from '@vben/common-ui';

import { Card, TabPane, Tabs } from 'ant-design-vue';

import { listAll, policiesApi } from '#/api/backend';
import { errorToast } from '#/composables/api';

import AccountsTab from './components/accounts-tab.vue';
import CodesTab from './components/codes-tab.vue';
import EnforcementsTab from './components/enforcements-tab.vue';
import LedgerTab from './components/ledger-tab.vue';

const TABS = ['accounts', 'ledger', 'codes', 'enforcements'] as const;

const route = useRoute();
function queryText(k: string): string | undefined {
  const v = route.query[k];
  return typeof v === 'string' && v ? v : undefined;
}
const userId = queryText('user_id');
const instanceId = queryText('instance_id');
const initialTab = queryText('tab');
const activeTab = ref<string>(TABS.find((t) => t === initialTab) ?? 'accounts');

// 额度政策版本：用于把 policy_version_id 显示为 `code v{version}`，以及切换政策的候选项。
const creditVersions = shallowRef<PolicyVersion[]>([]);

onMounted(async () => {
  try {
    creditVersions.value = await listAll((q) => policiesApi.list('credit', q));
  } catch (error) {
    errorToast(error);
  }
});
</script>

<template>
  <Page title="财务与额度">
    <Card size="small">
      <Tabs v-model:active-key="activeTab">
        <TabPane key="accounts" tab="额度账户">
          <AccountsTab :user-id="userId" :versions="creditVersions" />
        </TabPane>
        <TabPane key="ledger" tab="流水">
          <LedgerTab :instance-id="instanceId" :user-id="userId" />
        </TabPane>
        <TabPane key="codes" tab="兑换码">
          <CodesTab />
        </TabPane>
        <TabPane key="enforcements" tab="额度事件">
          <EnforcementsTab :user-id="userId" :versions="creditVersions" />
        </TabPane>
      </Tabs>
    </Card>
  </Page>
</template>
