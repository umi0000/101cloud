import type { Role, Task } from '#/api/types';

import { computed, h, ref } from 'vue';

import { notification } from 'ant-design-vue';
import { defineStore } from 'pinia';

import { memberApi, opsApi } from '#/api/backend';
import ErrorFeedback from '#/components/error-feedback.vue';
import { TASK_TYPES } from '#/locales/enums';
import { describeTaskError } from '#/locales/errors';

import { useAuthStore } from './auth';

interface Tracked {
  label: string;
  onDone?: () => void;
  startedAt: number;
  task: null | Task;
  requestId: string;
  taskId: string;
  role: Role;
  userId: string;
}

const TERMINAL: Record<string, true> = { cancelled: true, failed: true, succeeded: true };

/**
 * 任务中心（frontend/前端设计.md 5.3）。按发起账号角色选择本人或管理端查询接口。
 * 前 30 秒每 2 秒、之后每 5 秒；unknown 每 15 秒；终态停止。
 * unknown 不是终态，前端不重新发起同样的操作。
 */
export const useTaskStore = defineStore('tasks', () => {
  const authStore = useAuthStore();
  const tracked = ref<Tracked[]>([]);
  const open = ref(false);
  const timers = new Map<string, number>();
  let generation = 0;

  const active = computed(() =>
    tracked.value.filter((t) => !t.task || !TERMINAL[t.task.status]),
  );

  function interval(t: Tracked): number {
    if (t.task?.status === 'unknown') return 15_000;
    return Date.now() - t.startedAt < 30_000 ? 2000 : 5000;
  }

  async function poll(taskId: string) {
    const t = tracked.value.find((x) => x.taskId === taskId);
    if (!t || authStore.me?.user.id !== t.userId || authStore.me.user.role !== t.role) return;
    const mine = generation;
    try {
      const r = await (t.role === 'admin' ? opsApi.task(taskId) : memberApi.task(taskId));
      if (mine !== generation || !tracked.value.includes(t)) return;
      const before = t.task?.status;
      t.task = r.data;
      t.requestId = r.requestId;
      if (TERMINAL[r.data.status]) {
        timers.delete(taskId);
        if (before !== r.data.status) {
          const name = TASK_TYPES[r.data.type] ?? t.label;
          if (r.data.status === 'succeeded') {
            notification.success({ duration: 4, message: `${name}：成功` });
          } else {
            notification.error({
              description: () => h(ErrorFeedback, { feedback: describeTaskError(r.data, r.requestId) }),
              duration: 8,
              message: `${name}：${r.data.status === 'failed' ? '失败' : '已取消'}`,
            });
          }
        }
        t.onDone?.();
        return;
      }
    } catch {
      // 查询失败下次再试
    }
    if (mine !== generation || !tracked.value.includes(t)) return;
    timers.set(taskId, window.setTimeout(() => void poll(taskId), interval(t)));
  }

  function track(taskId: string, label: string, onDone?: () => void) {
    const user = authStore.me?.user;
    if (!user) return;
    const existing = tracked.value.find((x) => x.taskId === taskId);
    if (existing) {
      if (onDone) existing.onDone = onDone;
      return;
    }
    tracked.value.unshift({ label, onDone, requestId: '', role: user.role, startedAt: Date.now(), task: null, taskId, userId: user.id });
    if (tracked.value.length > 50) {
      const removed = tracked.value.splice(50);
      for (const r of removed) {
        window.clearTimeout(timers.get(r.taskId));
        timers.delete(r.taskId);
      }
    }
    void poll(taskId);
  }

  function clearFinished() {
    tracked.value = tracked.value.filter((t) => !t.task || !TERMINAL[t.task.status]);
  }

  function $reset() {
    generation++;
    for (const t of timers.values()) window.clearTimeout(t);
    timers.clear();
    tracked.value = [];
    open.value = false;
  }

  return { $reset, active, clearFinished, open, track, tracked };
});
