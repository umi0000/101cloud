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
];

export default routes;
