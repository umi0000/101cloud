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
    defaultHomePath: '/',
    enableCheckUpdates: false,
    enablePreferences: true,
    enableRefreshToken: false,
    locale: 'zh-CN',
    loginExpiredMode: 'modal',
    name: import.meta.env.VITE_APP_TITLE,
    timezone: 'Asia/Shanghai',
    watermark: false,
    layout: 'header-sidebar-nav',
  },
  copyright: {
    enable: false,
  },
  logo: {
    enable: true,
    source: '/brill-logo.png',
  },
  shortcutKeys: {
    globalLockScreen: false,
  },
  tabbar: {
    enable: false,
    persist: false,
  },
  theme: {
    mode: 'auto',
    semiDarkHeader: false,
    semiDarkSidebar: false,
    semiDarkSidebarSub: false,
    builtinType: 'pink',
    colorPrimary: 'hsl(347 77% 60%)',
  },
  widget: {
    globalSearch: false,
    languageToggle: false,
    lockScreen: false,
    notification: false,
    timezone: false,
    fullscreen: false,
  },
});