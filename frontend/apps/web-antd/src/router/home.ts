import type { Role } from '#/api/types';

export function getHomePath(role: Role): string {
  return role === 'admin' ? '/admin/dashboard' : '/instances';
}

/** 登录回跳只允许当前角色的站内业务页面；旧链接、外部地址回到角色首页。 */
export function getLoginRedirect(value: unknown, role: Role): string {
  const home = getHomePath(role);
  if (typeof value !== 'string') return home;
  try {
    const path = decodeURIComponent(value);
    if (!path.startsWith('/') || path.startsWith('//') || path.includes('\\')) return home;
    const url = new URL(path, 'http://localhost');
    const allowed = role === 'admin'
      ? url.pathname === '/admin' || url.pathname.startsWith('/admin/')
      : url.pathname === '/instances' || url.pathname === '/instances/create' || url.pathname === '/instances/create/confirm'
        || url.pathname === '/account/password'
        || /^\/instances\/\d+$/.test(url.pathname) || /^\/instances\/initialization\/\d+$/.test(url.pathname);
    return allowed ? `${url.pathname}${url.search}${url.hash}` : home;
  } catch {
    return home;
  }
}
