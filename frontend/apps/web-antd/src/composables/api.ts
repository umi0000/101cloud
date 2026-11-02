import type { Ref } from 'vue';

import type { Query } from '#/api/backend';
import type { ApiResult } from '#/api/request';
import type { Page, TaskAccepted } from '#/api/types';

import { h, onBeforeUnmount, ref, shallowRef, watch } from 'vue';

import { message, notification } from 'ant-design-vue';

import { ApiError, newIdempotencyKey } from '#/api/request';
import ErrorFeedback from '#/components/error-feedback.vue';
import { describeError } from '#/locales/errors';
import { useTaskStore } from '#/store/tasks';

export function errorToast(error: unknown) {
  const d = describeError(error);
  notification.error({
    description: () => h(ErrorFeedback, { feedback: d, hideMessage: true }),
    duration: 8,
    message: d.text,
  });
}

/**
 * 游标分页列表。筛选变化时清空游标栈回到第一页；invalid_cursor 时回到第一页。
 */
export function useCursorList<T, F extends Query = Query>(
  fetcher: (q: Query) => Promise<ApiResult<Page<T>>>,
  filters: Ref<F>,
  pageSize = 20,
) {
  const items = shallowRef<T[]>([]);
  const loading = ref(false);
  const stack = ref<(string | undefined)[]>([undefined]);
  const next = ref<null | string>(null);
  const requestId = ref('');
  let seq = 0;

  async function load() {
    const mine = ++seq;
    loading.value = true;
    try {
      const cursor = stack.value[stack.value.length - 1];
      const r = await fetcher({ ...filters.value, cursor, limit: pageSize });
      if (mine !== seq) return;
      items.value = r.data.items;
      next.value = r.data.next_cursor;
      requestId.value = r.requestId;
    } catch (error) {
      if (mine !== seq) return;
      if (error instanceof ApiError && error.code === 'invalid_cursor' && stack.value.length > 1) {
        stack.value = [undefined];
        await load();
        return;
      }
      errorToast(error);
    } finally {
      if (mine === seq) loading.value = false;
    }
  }

  function reset() {
    stack.value = [undefined];
    void load();
  }

  function nextPage() {
    if (!next.value) return;
    stack.value = [...stack.value, next.value];
    void load();
  }

  function prevPage() {
    if (stack.value.length <= 1) return;
    stack.value = stack.value.slice(0, -1);
    void load();
  }

  watch(filters, reset, { deep: true });

  return {
    hasNext: () => !!next.value,
    hasPrev: () => stack.value.length > 1,
    items,
    load,
    loading,
    nextPage,
    page: () => stack.value.length,
    prevPage,
    requestId,
    reset,
  };
}

/** 详情资源：数据 + ETag 一起缓存；每次写成功后调用 reload 刷新两者。fetcher 返回 null 表示当前无资源可加载（如未选中）。 */
export function useResource<T>(fetcher: () => null | Promise<ApiResult<T>>) {
  const data = shallowRef<null | T>(null);
  const etag = ref<null | string>(null);
  const loading = ref(false);
  const error = shallowRef<unknown>(null);
  const requestId = ref('');

  async function reload() {
    loading.value = true;
    try {
      const pending = fetcher();
      if (!pending) return;
      const r = await pending;
      data.value = r.data;
      etag.value = r.etag;
      requestId.value = r.requestId;
      error.value = null;
    } catch (error_) {
      error.value = error_;
      throw error_;
    } finally {
      loading.value = false;
    }
  }

  return { data, error, etag, loading, reload, requestId };
}

/**
 * 一次用户意图 = 一个幂等键。run() 首次调用生成键，网络错误/5xx 重试时复用；
 * 成功或确定性业务错误后丢弃，下次提交生成新键。412 时提示并刷新，不自动重提。
 */
export function useAction() {
  const pending = ref(false);
  let key: null | string = null;

  async function run<R>(
    fn: (key: string) => Promise<R>,
    options: { onConflict?: () => Promise<void> | void; success?: string } = {},
  ): Promise<R | undefined> {
    if (pending.value) return undefined;
    key ??= newIdempotencyKey();
    pending.value = true;
    try {
      const result = await fn(key);
      key = null;
      if (options.success) message.success(options.success);
      return result;
    } catch (error) {
      const retryable =
        !(error instanceof ApiError) || error.status === 0 || error.status >= 500;
      if (!retryable) key = null;
      if (error instanceof ApiError && error.code === 'revision_conflict') {
        await options.onConflict?.();
      }
      errorToast(error);
      return undefined;
    } finally {
      pending.value = false;
    }
  }

  /** 用户修改表单内容后调用，确保下次提交使用新键。 */
  function resetKey() {
    key = null;
  }

  return { pending, resetKey, run };
}

/** 202 受理后把任务交给任务中心跟踪。 */
export function trackAccepted(accepted: TaskAccepted | undefined, label: string, onDone?: () => void) {
  if (!accepted) return;
  useTaskStore().track(accepted.task_id, label, onDone);
}

/** 页面可见时周期执行；隐藏时暂停，恢复可见立即执行一次。 */
export function usePolling(fn: () => Promise<unknown> | void, intervalMs: () => number) {
  let timer: number | undefined;
  let stopped = false;

  async function tick() {
    if (stopped) return;
    if (!document.hidden) {
      try {
        await fn();
      } catch {
        // 单次失败不终止轮询
      }
    }
    if (stopped) return;
    timer = window.setTimeout(tick, intervalMs());
  }

  function onVisible() {
    if (!document.hidden) {
      window.clearTimeout(timer);
      void tick();
    }
  }

  document.addEventListener('visibilitychange', onVisible);
  timer = window.setTimeout(tick, intervalMs());

  onBeforeUnmount(() => {
    stopped = true;
    window.clearTimeout(timer);
    document.removeEventListener('visibilitychange', onVisible);
  });
}
