import { defineOverridesPreferences } from '@vben/preferences';

/**
 * 101cloud 偏好覆盖。见 frontend/前端设计.md 第 1、2 节。
 * 更改后需清空浏览器本地偏好缓存才生效。
 */
export const overridesPreferences = defineOverridesPreferences({
  app: {
    accessMode: 'frontend',
    compact: true,
    defaultAvatar: '',
    defaultHomePath: '/admin/dashboard',
    enableCheckUpdates: false,
    enablePreferences: true,
    enableRefreshToken: false,
    locale: 'zh-CN',
    loginExpiredMode: 'modal',
    name: import.meta.env.VITE_APP_TITLE,
    timezone: 'Asia/Shanghai',
    watermark: false,
  },
  copyright: {
    enable: false,
  },
  logo: {
    // 暂无 Logo，只显示文字品牌
    enable: true,
    source: '',
  },
  shortcutKeys: {
    globalLockScreen: false,
  },
  tabbar: {
    persist: false,
  },
  theme: {
    mode: 'light',
    semiDarkHeader: false,
    semiDarkSidebar: false,
    semiDarkSidebarSub: false,
  },
  widget: {
    globalSearch: false,
    languageToggle: false,
    lockScreen: false,
    notification: false,
    timezone: false,
  },
});
