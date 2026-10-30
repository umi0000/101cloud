import type { Router } from 'vue-router';

import { LOGIN_PATH } from '@vben/constants';
import { preferences } from '@vben/preferences';
import { useAccessStore } from '@vben/stores';
import { startProgress, stopProgress } from '@vben/utils';

import { accessRoutes, coreRouteNames } from '#/router/routes';
import { MEMBER_PENDING_PATH } from '#/router/routes/core';
import { useAuthStore } from '#/store';

import { generateAccess } from './access';

function setupCommonGuard(router: Router) {
  const loadedPaths = new Set<string>();
  router.beforeEach((to) => {
    to.meta.loaded = loadedPaths.has(to.path);
    if (!to.meta.loaded && preferences.transition.progress) {
      startProgress();
    }
    return true;
  });
  router.afterEach((to) => {
    loadedPaths.add(to.path);
    if (preferences.transition.progress) {
      stopProgress();
    }
  });
}

/**
 * 登录状态以 GET /me 为准（HttpOnly 会话 Cookie 前端不可读）。
 * 菜单按 user.role 过滤；后端对 /admin 接口另行强制 admin 角色。
 */
function setupAccessGuard(router: Router) {
  let probed = false;
  router.beforeEach(async (to) => {
    const accessStore = useAccessStore();
    const authStore = useAuthStore();

    if (!probed) {
      probed = true;
      await authStore.probe();
    }

    if (coreRouteNames.includes(to.name as string)) {
      if (to.path === LOGIN_PATH && authStore.me) {
        return authStore.isAdmin
          ? decodeURIComponent((to.query?.redirect as string) || preferences.app.defaultHomePath)
          : MEMBER_PENDING_PATH;
      }
      return true;
    }

    if (!authStore.me) {
      if (to.meta.ignoreAccess) return true;
      return {
        path: LOGIN_PATH,
        query:
          to.fullPath === preferences.app.defaultHomePath
            ? {}
            : { redirect: encodeURIComponent(to.fullPath) },
        replace: true,
      };
    }

    // 成员端尚未实现：非管理员只能看到占位页（后端仍对 /admin 接口强制 admin 角色）。
    if (!authStore.isAdmin) {
      return to.path === MEMBER_PENDING_PATH ? true : { path: MEMBER_PENDING_PATH, replace: true };
    }

    if (accessStore.isAccessChecked) {
      return true;
    }

    const { accessibleMenus, accessibleRoutes } = await generateAccess({
      roles: [authStore.me.user.role],
      router,
      routes: accessRoutes,
    });
    accessStore.setAccessMenus(accessibleMenus);
    accessStore.setAccessRoutes(accessibleRoutes);
    accessStore.setIsAccessChecked(true);
    return { ...router.resolve(to.fullPath), replace: true };
  });
}

function createRouterGuard(router: Router) {
  setupCommonGuard(router);
  setupAccessGuard(router);
}

export { createRouterGuard };
