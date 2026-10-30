import { defineConfig } from '@vben/vite-config';

import { loadEnv } from 'vite';

export default defineConfig(async (config) => {
  const env = loadEnv(config?.mode ?? 'development', process.cwd());
  const target = env.VITE_DEV_PROXY_TARGET || 'http://127.0.0.1:8080';
  return {
    application: {
      nitroMock: false,
    },
    vite: {
      server: {
        // Loopback only: the dev backend accepts Origin http://localhost:5555 and plain-http cookies.
        host: 'localhost',
        // Same-origin dev proxy: path is not rewritten and the browser Origin
        // header is forwarded unchanged, so the backend Origin/CSRF checks apply.
        proxy: {
          '/api/v1': {
            changeOrigin: true,
            target,
            ws: true,
          },
        },
        strictPort: true,
      },
    },
  };
});
