<script lang="ts" setup>
import { computed, ref } from 'vue';
import { useRouter } from 'vue-router';

import { AuthenticationLoginExpiredModal } from '@vben/common-ui';
import { BasicLayout, UserDropdown } from '@vben/layouts';
import { useAccessStore } from '@vben/stores';

import MemberBalance from '#/components/member-balance.vue';
import TaskCenter from '#/components/task-center.vue';
import { enumLabel } from '#/locales/enums';
import { useAuthStore } from '#/store';
import LoginForm from '#/views/_core/authentication/login.vue';

const authStore = useAuthStore();
const accessStore = useAccessStore();
const router = useRouter();
const userMenus = computed(() => memberUserId.value ? [{
  text: '修改密码',
  icon: 'lucide:key-round',
  handler: () => router.push({ name: 'MemberPasswordChange' }),
}] : []);
const loggingOut = ref(false);
const memberUserId = computed(() =>
  authStore.me?.user.role === 'user' && !accessStore.loginExpired && !loggingOut.value
    ? authStore.me.user.id
    : null,
);

const username = computed(() => authStore.me?.user.username ?? '');
const roleText = computed(() => enumLabel('role', authStore.me?.user.role).text);

async function handleLogout() {
  loggingOut.value = true;
  try {
    await authStore.logout();
  } finally {
    loggingOut.value = false;
  }
}
</script>

<template>
  <BasicLayout :key="`${authStore.me?.user.id}:${authStore.me?.user.role}`" @clear-preferences-and-logout="handleLogout">
    <template #header-right-0>
      <MemberBalance v-if="memberUserId" :key="memberUserId" />
      <TaskCenter v-if="authStore.me && !accessStore.loginExpired && !loggingOut" />
    </template>
    <template #user-dropdown>
      <UserDropdown
        avatar=""
        :description="roleText"
        :enable-shortcut-key="false"
        :menus="userMenus"
        :text="username"
        @logout="handleLogout"
      />
    </template>
    <template #extra>
      <AuthenticationLoginExpiredModal v-model:open="accessStore.loginExpired" avatar="">
        <LoginForm embedded />
      </AuthenticationLoginExpiredModal>
    </template>
  </BasicLayout>
</template>
