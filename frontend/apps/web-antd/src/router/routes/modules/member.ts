import type { RouteRecordRaw } from 'vue-router';

const routes: RouteRecordRaw[] = [
  {
    component: () => import('#/views/member/instances/index.vue'),
    meta: {
      authority: ['user'],
      icon: 'lucide:box',
      title: '实例管理',
    },
    name: 'MemberInstances',
    path: '/instances',
  },
  {
    component: () => import('#/views/member/instances/create.vue'),
    meta: {
      activePath: '/instances',
      authority: ['user'],
      hideInMenu: true,
      title: '创建弹性容器',
    },
    name: 'MemberInstanceCreate',
    path: '/instances/create',
  },
  {
    component: () => import('#/views/member/instances/confirm.vue'),
    meta: {
      activePath: '/instances',
      authority: ['user'],
      hideInMenu: true,
      title: '确认订单',
    },
    name: 'MemberInstanceConfirm',
    path: '/instances/create/confirm',
  },
  {
    component: () => import('#/views/member/instances/initialization.vue'),
    meta: {
      activePath: '/instances',
      authority: ['user'],
      hideInMenu: true,
      title: '实例初始化',
    },
    name: 'MemberInstanceInitialization',
    path: '/instances/initialization/:taskId',
  },
];

export default routes;
