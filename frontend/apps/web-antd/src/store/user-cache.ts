import { reactive } from 'vue';

import { defineStore } from 'pinia';

import { usersApi } from '#/api/backend';

/** 管理端 user_id → 用户名。后端无批量查询，逐个 GET /admin/users/{id} 并在会话内缓存。 */
export const useUserCache = defineStore('user-cache', () => {
  const names = reactive<Record<string, string>>({});
  const inflight = new Set<string>();

  function ensure(id: string) {
    if (names[id] || inflight.has(id)) return;
    inflight.add(id);
    usersApi
      .get(id)
      .then((r) => {
        names[id] = r.data.username;
      })
      .catch(() => {
        names[id] = `#${id}`;
      })
      .finally(() => inflight.delete(id));
  }

  function put(id: string, username: string) {
    names[id] = username;
  }

  function $reset() {
    for (const k of Object.keys(names)) delete names[k];
  }

  return { $reset, ensure, names, put };
});
