import type { RouteRecordRaw } from 'vue-router';

const admin = { authority: ['admin'] };

/** 管理端菜单，顺序见 frontend/前端设计.md 第 7 节。 */
const routes: RouteRecordRaw[] = [
  {
    meta: {
      ...admin,
      hideInMenu: true,
      hideInBreadcrumb: true,
      title: '管理入口',
    },
    name: 'Admin',
    path: '/admin',
    redirect: '/admin/dashboard',
  },
  {
    component: () => import('#/views/admin/dashboard/index.vue'),
    meta: {
      ...admin,
      affixTab: true,
      icon: 'lucide:layout-dashboard',
      title: '概览',
    },
    name: 'AdminDashboard',
    path: '/admin/dashboard',
  },
  {
    component: () => import('#/views/admin/users/index.vue'),
    meta: { ...admin, icon: 'lucide:users', title: '用户与配额' },
    name: 'AdminUsers',
    path: '/admin/users',
  },
  {
    component: () => import('#/views/admin/users/detail.vue'),
    meta: {
      ...admin,
      activePath: '/admin/users',
      hideInMenu: true,
      title: '用户详情',
    },
    name: 'AdminUserDetail',
    path: '/admin/users/:id',
  },
  {
    component: () => import('#/views/admin/nodes/index.vue'),
    meta: { ...admin, icon: 'lucide:server', title: '节点与 GPU' },
    name: 'AdminNodes',
    path: '/admin/nodes',
  },
  {
    component: () => import('#/views/admin/nodes/detail.vue'),
    meta: {
      ...admin,
      activePath: '/admin/nodes',
      hideInMenu: true,
      title: '节点详情',
    },
    name: 'AdminNodeDetail',
    path: '/admin/nodes/:id',
  },
  {
    component: () => import('#/views/admin/instances/index.vue'),
    meta: { ...admin, icon: 'lucide:box', title: '实例' },
    name: 'AdminInstances',
    path: '/admin/instances',
  },
  {
    component: () => import('#/views/admin/instances/detail.vue'),
    meta: {
      ...admin,
      activePath: '/admin/instances',
      hideInMenu: true,
      title: '实例详情',
    },
    name: 'AdminInstanceDetail',
    path: '/admin/instances/:id',
  },
  {
    component: () => import('#/views/admin/requests/index.vue'),
    meta: { ...admin, icon: 'lucide:clipboard-check', title: '自定义申请' },
    name: 'AdminRequests',
    path: '/admin/requests',
  },
  {
    component: () => import('#/views/admin/requests/detail.vue'),
    meta: {
      ...admin,
      activePath: '/admin/requests',
      hideInMenu: true,
      title: '申请审批',
    },
    name: 'AdminRequestDetail',
    path: '/admin/requests/:id',
  },
  {
    component: () => import('#/views/admin/catalog/index.vue'),
    meta: { ...admin, icon: 'lucide:package', title: '产品目录' },
    name: 'AdminCatalog',
    path: '/admin/catalog',
  },
  {
    component: () => import('#/views/admin/network/index.vue'),
    meta: { ...admin, icon: 'lucide:network', title: '网络与 IP' },
    name: 'AdminNetwork',
    path: '/admin/network',
  },
  {
    component: () => import('#/views/admin/policies/index.vue'),
    meta: { ...admin, icon: 'lucide:badge-dollar-sign', title: '政策与定价' },
    name: 'AdminPolicies',
    path: '/admin/policies',
  },
  {
    component: () => import('#/views/admin/finance/index.vue'),
    meta: { ...admin, icon: 'lucide:wallet', title: '财务与额度' },
    name: 'AdminFinance',
    path: '/admin/finance',
  },
  {
    component: () => import('#/views/admin/tasks/index.vue'),
    meta: { ...admin, icon: 'lucide:list-checks', title: '任务' },
    name: 'AdminTasks',
    path: '/admin/tasks',
  },
  {
    component: () => import('#/views/admin/audit/index.vue'),
    meta: { ...admin, icon: 'lucide:scroll-text', title: '审计日志' },
    name: 'AdminAudit',
    path: '/admin/audit',
  },
];

export default routes;
