<script lang="ts" setup>
import { ref, watch } from 'vue';
import { useRoute, useRouter } from 'vue-router';

import { Page } from '@vben/common-ui';

import { Alert, Button, Card, Skeleton, Space, TabPane, Tabs } from 'ant-design-vue';

import { usersApi } from '#/api/backend';
import ErrorFeedback from '#/components/error-feedback.vue';
import StatusTag from '#/components/status-tag.vue';
import { errorToast, useResource } from '#/composables/api';
import { describeError } from '#/locales/errors';
import { useUserCache } from '#/store/user-cache';

import CreditTab from './components/credit-tab.vue';
import InstancesTab from './components/instances-tab.vue';
import LimitsTab from './components/limits-tab.vue';
import PasswordTab from './components/password-tab.vue';
import ProfileTab from './components/profile-tab.vue';

const TABS = ['profile', 'password', 'limits', 'credit', 'instances'] as const;
type Tab = (typeof TABS)[number];

const route = useRoute();
const router = useRouter();
const cache = useUserCache();

/** 离开本页时 route 先变化，watcher 仍可能触发一次；只在本路由内响应。 */
const onPage = () => route.path.startsWith('/admin/users/') && typeof route.params.id === 'string';
const id = ref(String(route.params.id));
const user = useResource(() => usersApi.get(id.value));

function tabFrom(q: unknown): Tab {
  return TABS.find((t) => t === q) ?? 'profile';
}
const tab = ref<Tab>(tabFrom(route.query.tab));
watch(
  () => [route.params.id, route.query.tab],
  () => {
    if (!onPage()) return;
    id.value = String(route.params.id);
    tab.value = tabFrom(route.query.tab);
  },
);
watch(tab, (t) => {
  if (onPage() && route.query.tab !== t) void router.replace({ query: { ...route.query, tab: t } });
});

async function reload() {
  try {
    await user.reload();
  } catch (error) {
    errorToast(error);
  }
}
watch(user.data, (u) => u && cache.put(u.id, u.username));
watch(id, reload, { immediate: true });
</script>

<template>
  <Page :title="user.data.value ? `用户 ${user.data.value.username}` : '用户详情'">
    <template #extra>
      <Space v-if="user.data.value">
        <span class="text-muted-foreground font-mono text-xs">#{{ user.data.value.id }}</span>
        <StatusTag kind="role" :value="user.data.value.role" />
        <StatusTag kind="userStatus" :value="user.data.value.status" />
      </Space>
    </template>
    <Card size="small">
      <Alert
        v-if="user.error.value && !user.data.value"
        :message="describeError(user.error.value).text"
        show-icon
        type="error"
      >
        <template #description><ErrorFeedback :feedback="describeError(user.error.value)" hide-message /></template>
        <template #action><Button size="small" @click="reload">重试</Button></template>
      </Alert>
      <Skeleton v-else-if="!user.data.value" active />
      <Tabs v-else :key="id" v-model:active-key="tab">
        <TabPane key="profile" tab="资料">
          <ProfileTab :etag="user.etag.value" :user="user.data.value" @changed="reload" />
        </TabPane>
        <TabPane key="password" tab="重置密码">
          <PasswordTab :etag="user.etag.value" :user="user.data.value" @changed="reload" />
        </TabPane>
        <TabPane key="limits" tab="配额"><LimitsTab :user-id="id" /></TabPane>
        <TabPane key="credit" tab="额度账户"><CreditTab :user-id="id" /></TabPane>
        <TabPane key="instances" tab="实例"><InstancesTab :user-id="id" /></TabPane>
      </Tabs>
    </Card>
  </Page>
</template>
