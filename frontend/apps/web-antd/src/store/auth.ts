import type { Me } from '#/api/types';

import { computed, ref } from 'vue';

import { LOGIN_PATH } from '@vben/constants';
import { resetAllStores, useAccessStore, useUserStore } from '@vben/stores';

import { defineStore } from 'pinia';

import { authApi } from '#/api/backend';
import { ApiError, onUnauthenticated, refreshCsrf } from '#/api/request';
import { router } from '#/router';

/**
 * 服务端 Session 认证：浏览器保存 HttpOnly Cookie，前端只以 GET /me 判断登录状态。
 * 不使用 accessToken / refresh token。
 */
export const useAuthStore = defineStore('auth', () => {
  const accessStore = useAccessStore();
  const userStore = useUserStore();

  const me = ref<Me | null>(null);
  const loginLoading = ref(false);
  const isAdmin = computed(() => me.value?.user.role === 'admin');

  function applyMe(value: Me) {
    me.value = value;
    userStore.setUserInfo({
      avatar: '',
      realName: value.user.username,
      roles: [value.user.role],
      userId: value.user.id,
      username: value.user.username,
    });
    accessStore.setAccessCodes(value.effective_permissions);
  }

  /** 启动或刷新时探测会话；401 返回 false，不触发过期弹窗。 */
  async function probe(): Promise<boolean> {
    try {
      const r = await authApi.me(true);
      applyMe(r.data);
      return true;
    } catch (error) {
      if (error instanceof ApiError && error.status === 401) return false;
      throw error;
    }
  }

  /** 登录：先取 CSRF，再提交；成功后改用登录响应里轮换的 token（请求层自动替换）。 */
  async function login(username: string, password: string) {
    loginLoading.value = true;
    try {
      await refreshCsrf();
      await authApi.login(username, password);
      const r = await authApi.me(true);
      applyMe(r.data);
      accessStore.setLoginExpired(false);
    } finally {
      loginLoading.value = false;
    }
  }

  /** resetAllStores() 会对每个 store 调用 $reset；setup store 必须自行提供。 */
  function $reset() {
    me.value = null;
    loginLoading.value = false;
  }

  function clear() {
    resetAllStores();
  }

  async function logout() {
    try {
      await authApi.logout();
    } catch {
      // 会话可能已失效，忽略
    }
    clear();
    accessStore.setLoginExpired(false);
    await router.replace(LOGIN_PATH);
  }

  /** 会话被撤销或过期后，回到登录页；弹窗模式仅在已进入应用时使用。 */
  onUnauthenticated(() => {
    if (me.value && accessStore.isAccessChecked) {
      accessStore.setLoginExpired(true);
      return;
    }
    clear();
    if (router.currentRoute.value.path !== LOGIN_PATH) {
      void router.replace(LOGIN_PATH);
    }
  });

  return { $reset, clear, isAdmin, login, loginLoading, logout, me, probe };
});
