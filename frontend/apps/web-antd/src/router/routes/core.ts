import type { RouteRecordRaw } from 'vue-router';

import { LOGIN_PATH } from '@vben/constants';
import { preferences } from '@vben/preferences';

const BasicLayout = () => import('#/layouts/basic.vue');
const AuthPageLayout = () => import('#/layouts/auth.vue');

const fallbackNotFoundRoute: RouteRecordRaw = {
  component: () => import('#/views/_core/fallback/not-found.vue'),
  meta: {
    hideInBreadcrumb: true,
    hideInMenu: true,
    hideInTab: true,
    title: '404',
  },
  name: 'FallbackNotFound',
  path: '/:path(.*)*',
};

/** 成员端尚未实现时，非管理员登录后进入的占位页。 */
export const MEMBER_PENDING_PATH = '/member-pending';
/** 基本路由。只有用户名密码登录；没有注册、验证码、第三方登录。 */
const coreRoutes: RouteRecordRaw[] = [
  {
    component: BasicLayout,
    meta: { hideInBreadcrumb: true, title: 'Root' },
    name: 'Root',
    path: '/',
    redirect: preferences.app.defaultHomePath,
    children: [],
  },
  {
    component: AuthPageLayout,
    meta: { hideInTab: true, title: '登录' },
    name: 'Authentication',
    path: '/auth',
    redirect: LOGIN_PATH,
    children: [
      {
        component: () => import('#/views/_core/authentication/login.vue'),
        meta: { title: '登录' },
        name: 'Login',
        path: 'login',
      },
    ],
  },
  {
    component: AuthPageLayout,
    meta: { hideInTab: true, title: '成员端暂未开放' },
    name: 'MemberPending',
    path: MEMBER_PENDING_PATH,
    children: [
      {
        component: () => import('#/views/_core/fallback/member-pending.vue'),
        meta: { title: '成员端暂未开放' },
        name: 'MemberPendingPage',
        path: '',
      },
    ],
  },
];

export { coreRoutes, fallbackNotFoundRoute };
