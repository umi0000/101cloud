import type { Role, Task } from '#/api/types';
import type { ErrorText } from '#/locales/errors';

import { computed, h, ref } from 'vue';

import { notification } from 'ant-design-vue';
import { defineStore } from 'pinia';

import { memberApi, opsApi } from '#/api/backend';
import { ApiError, newIdempotencyKey, revisionTag } from '#/api/request';
import ErrorFeedback from '#/components/error-feedback.vue';
import { TASK_TYPES } from '#/locales/enums';
import { describeError, describeTaskError } from '#/locales/errors';
import { creationState } from '#/utils/creation-progress';

import { useAuthStore } from './auth';

interface Tracked {
  etag: null | string;
  pollVersion: number;
  stop: {
    pending: boolean;
    uncertain: boolean;
    feedback: null | ErrorText;
    intent: null | { etag: string; key: string };
  };
  label: string;
  onDone?: () => void;
  startedAt: number;
  task: null | Task;
  requestId: string;
  taskId: string;
  role: Role;
  userId: string;
}

export interface TaskStopSource {
  task: Task;
  etag: null | string;
  onDone?: () => void;
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

  function owns(t: Tracked) {
    const user = authStore.me?.user;
    return user?.id === t.userId && user.role === t.role;
  }

  function current(t: Tracked, mine: number, version: number) {
    return mine === generation && t.pollVersion === version && tracked.value.includes(t) && owns(t);
  }
  async function poll(taskId: string) {
    const t = tracked.value.find((x) => x.taskId === taskId);
    if (!t || !owns(t) || t.stop.pending) return;
    window.clearTimeout(timers.get(taskId));
    timers.delete(taskId);
    const mine = generation;
    const version = ++t.pollVersion;
    try {
      const r = await (t.role === 'admin' ? opsApi.task(taskId) : memberApi.task(taskId));
      if (!current(t, mine, version)) return;
      const before = t.task?.status;
      t.task = r.data;
      t.requestId = r.requestId;
      t.etag = r.etag ?? revisionTag(r.data.revision);
      if (TERMINAL[r.data.status]) {
        t.stop.intent = null;
        t.stop.uncertain = false;
        timers.delete(taskId);
        if (before !== r.data.status) {
          const name = TASK_TYPES[r.data.type] ?? t.label;
          if (r.data.status === 'succeeded') {
            if (r.data.type === 'instance.create' && creationState(r.data) !== 'succeeded') {
              notification.warning({ duration: 6, message: `${name}：初始化尚未确认完成` });
            } else {
              notification.success({ duration: 4, message: `${name}：成功` });
            }
          } else {
            notification.error({
              description: () => h(ErrorFeedback, { feedback: describeTaskError(r.data, r.requestId) }),
              duration: 8,
              message: `${name}：${r.data.status === 'failed' ? '失败' : '已取消'}`,
            });
          }
        }
        if (!before || !TERMINAL[before]) t.onDone?.();
        return;
      }
    } catch {
      // 查询失败下次再试
    }
    if (!current(t, mine, version)) return;
    timers.set(taskId, window.setTimeout(() => void poll(taskId), interval(t)));
  }

  /** A retry keeps the original task version and key, even if polling has moved on. */
  async function forceStop(taskId: string, source?: TaskStopSource): Promise<boolean> {
    if (source && source.task.id !== taskId) return false;
    let t = tracked.value.find((item) => item.taskId === taskId);
    if (!t && source && !TERMINAL[source.task.status] && source.task.can_force_stop !== false) {
      t = register(taskId, TASK_TYPES[source.task.type] ?? source.task.type, source.onDone, source);
    }
    if (!t || !owns(t) || t.stop.pending) return false;
    if (source?.onDone) t.onDone = source.onDone;
    if (source && !t.stop.intent) {
      t.task = source.task;
      t.etag = source.etag;
    }
    if (!t.stop.intent) {
      if (!t.task || TERMINAL[t.task.status] || t.task.can_force_stop === false) return false;
      t.stop.intent = { etag: t.etag ?? revisionTag(t.task.revision), key: newIdempotencyKey() };
    }
    const intent = t.stop.intent;
    const retrying = t.stop.uncertain;
    const mine = generation;
    const version = ++t.pollVersion;
    window.clearTimeout(timers.get(taskId));
    timers.delete(taskId);
    t.stop.pending = true;
    t.stop.feedback = null;
    let stopped = false;
    try {
      const response = await (t.role === 'admin' ? opsApi : memberApi).forceStopTask(taskId, { ifMatch: intent.etag, key: intent.key });
      if (!current(t, mine, version)) return false;
      t.task = response.data;
      t.etag = response.etag ?? revisionTag(response.data.revision);
      t.requestId = response.requestId;
      t.stop.intent = null;
      t.stop.uncertain = false;
      stopped = true;
      notification.success({ message: response.data.force_stop?.orchestration_stopped ? '任务编排已强制停止' : '任务已结束，无需停止', duration: 5 });
    } catch (failure) {
      if (!current(t, mine, version)) return false;
      t.stop.feedback = describeError(failure);
      t.stop.uncertain = !(failure instanceof ApiError) || failure.code === 'invalid_response' || failure.status === 0 || failure.status === 408 || failure.status >= 500
        || (retrying && [401, 403, 429].includes(failure.status));
      if (!t.stop.uncertain) t.stop.intent = null;
    } finally {
      if (current(t, mine, version)) {
        t.stop.pending = false;
        // Fetch the authoritative task again; an older in-flight poll cannot overwrite this response.
        void poll(taskId);
      }
    }
    if (stopped && mine === generation && tracked.value.includes(t) && owns(t)) t.onDone?.();
    return stopped;
  }

  function register(taskId: string, label: string, onDone?: () => void, source?: TaskStopSource) {
    const user = authStore.me?.user;
    if (!user) return;
    const existing = tracked.value.find((x) => x.taskId === taskId);
    if (existing) {
      if (onDone) existing.onDone = onDone;
      return existing;
    }
    tracked.value.unshift({ etag: source?.etag ?? null, pollVersion: 0, stop: { pending: false, uncertain: false, feedback: null, intent: null }, label, onDone, requestId: '', role: user.role, startedAt: Date.now(), task: source?.task ?? null, taskId, userId: user.id });
    if (tracked.value.length > 50) {
      const removed = tracked.value.splice(50);
      for (const r of removed) {
        window.clearTimeout(timers.get(r.taskId));
        timers.delete(r.taskId);
      }
    }
    return tracked.value[0];
  }

  function track(taskId: string, label: string, onDone?: () => void) {
    const existing = tracked.value.some((item) => item.taskId === taskId);
    const task = register(taskId, label, onDone);
    if (task && !existing) void poll(taskId);
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

  return { $reset, active, clearFinished, forceStop, open, track, tracked };
});
