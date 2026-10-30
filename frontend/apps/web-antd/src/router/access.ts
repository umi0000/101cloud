import type {
  ComponentRecordType,
  GenerateMenuAndRoutesOptions,
} from '@vben/types';

import { generateAccessible } from '@vben/access';

import { BasicLayout, IFrameView } from '#/layouts';

const forbiddenComponent = () => import('#/views/_core/fallback/forbidden.vue');

/** 前端路由模式：路由写死，按 meta.authority 与用户角色过滤，不请求后端菜单。 */
async function generateAccess(options: GenerateMenuAndRoutesOptions) {
  const pageMap: ComponentRecordType = import.meta.glob('../views/**/*.vue');
  const layoutMap: ComponentRecordType = { BasicLayout, IFrameView };
  return await generateAccessible('frontend', {
    ...options,
    forbiddenComponent,
    layoutMap,
    pageMap,
  });
}

export { generateAccess };
