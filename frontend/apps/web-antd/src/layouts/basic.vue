<script lang="ts" setup>
import { computed } from 'vue';

import { AuthenticationLoginExpiredModal } from '@vben/common-ui';
import { BasicLayout, UserDropdown } from '@vben/layouts';
import { useAccessStore } from '@vben/stores';

import TaskCenter from '#/components/task-center.vue';
import { enumLabel } from '#/locales/enums';
import { useAuthStore } from '#/store';
import LoginForm from '#/views/_core/authentication/login.vue';

const authStore = useAuthStore();
const accessStore = useAccessStore();

const username = computed(() => authStore.me?.user.username ?? '');
const roleText = computed(() => enumLabel('role', authStore.me?.user.role).text);

async function handleLogout() {
  await authStore.logout();
}
</script>

<template>
  <BasicLayout @clear-preferences-and-logout="handleLogout">
    <template #header-right-0>
      <TaskCenter />
    </template>
    <template #user-dropdown>
      <UserDropdown
        avatar=""
        :description="roleText"
        :enable-shortcut-key="false"
        :menus="[]"
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
