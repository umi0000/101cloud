import type { RouteRecordRaw } from 'vue-router';

import { LOGIN_PATH } from '@vben/constants';

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

/** 基本路由：用户名密码登录与公开注册入口。 */
const coreRoutes: RouteRecordRaw[] = [
  {
    component: BasicLayout,
    meta: { hideInBreadcrumb: true, title: 'Root' },
    name: 'Root',
    path: '/',
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
      {
        component: () => import('#/views/_core/authentication/register.vue'),
        meta: { title: '注册' },
        name: 'Register',
        path: 'register',
      },
    ],
  },
];

export { coreRoutes, fallbackNotFoundRoute };
