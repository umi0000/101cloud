import type { Task } from '#/api/types';

import { computed, ref } from 'vue';

import { notification } from 'ant-design-vue';
import { defineStore } from 'pinia';

import { opsApi } from '#/api/backend';
import { TASK_TYPES } from '#/locales/enums';

interface Tracked {
  label: string;
  onDone?: () => void;
  startedAt: number;
  task: null | Task;
  taskId: string;
}

const TERMINAL: Record<string, true> = { cancelled: true, failed: true, succeeded: true };

/**
 * 任务中心（frontend/前端设计.md 5.3）。管理员发起的任务用 /admin/tasks/{id} 查询。
 * 前 30 秒每 2 秒、之后每 5 秒；unknown 每 15 秒；终态停止。
 * unknown 不是终态，前端不重新发起同样的操作。
 */
export const useTaskStore = defineStore('tasks', () => {
  const tracked = ref<Tracked[]>([]);
  const open = ref(false);
  const timers = new Map<string, number>();

  const active = computed(() =>
    tracked.value.filter((t) => !t.task || !TERMINAL[t.task.status]),
  );

  function interval(t: Tracked): number {
    if (t.task?.status === 'unknown') return 15_000;
    return Date.now() - t.startedAt < 30_000 ? 2000 : 5000;
  }

  async function poll(taskId: string) {
    const t = tracked.value.find((x) => x.taskId === taskId);
    if (!t) return;
    try {
      const r = await opsApi.task(taskId);
      const before = t.task?.status;
      t.task = r.data;
      if (TERMINAL[r.data.status]) {
        timers.delete(taskId);
        if (before !== r.data.status) {
          const name = TASK_TYPES[r.data.type] ?? t.label;
          if (r.data.status === 'succeeded') {
            notification.success({ duration: 4, message: `${name}：成功` });
          } else {
            notification.error({
              description: r.data.error_code ?? undefined,
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
    timers.set(taskId, window.setTimeout(() => void poll(taskId), interval(t)));
  }

  function track(taskId: string, label: string, onDone?: () => void) {
    const existing = tracked.value.find((x) => x.taskId === taskId);
    if (existing) {
      if (onDone) existing.onDone = onDone;
      return;
    }
    tracked.value.unshift({ label, onDone, startedAt: Date.now(), task: null, taskId });
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
    for (const t of timers.values()) window.clearTimeout(t);
    timers.clear();
    tracked.value = [];
    open.value = false;
  }

  return { $reset, active, clearFinished, open, track, tracked };
});
