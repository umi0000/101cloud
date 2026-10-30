/** 实例页内部共用的小工具与中文标签。 */
import { onBeforeUnmount, ref } from 'vue';

export const ACCESS_MODE_NAMES: Record<string, string> = { exclusive: '独占', shared: '共享' };

export const GPU_STATE_NAMES: Record<string, string> = {
  idle_candidate: '空闲候选',
  in_use: '使用中',
  not_attached: '未挂载',
  unknown: '未知',
};

/** 定时刷新的当前时间（毫秒），组件卸载时停止。 */
export function useNow(intervalMs = 1000) {
  const now = ref(Date.now());
  const timer = window.setInterval(() => (now.value = Date.now()), intervalMs);
  onBeforeUnmount(() => window.clearInterval(timer));
  return now;
}

/** 剩余毫秒 → "mm:ss"，超过 1 小时为 "h:mm:ss"；已到期为 "00:00"。 */
export function formatCountdown(ms: number): string {
  const total = Math.max(0, Math.floor(ms / 1000));
  const h = Math.floor(total / 3600);
  const mmss = `${String(Math.floor((total % 3600) / 60)).padStart(2, '0')}:${String(total % 60).padStart(2, '0')}`;
  return h > 0 ? `${h}:${mmss}` : mmss;
}

/** 布尔或 null → 是/否/未知。 */
export function yesNo(value: boolean | null | undefined): string {
  if (value === null || value === undefined) return '未知';
  return value ? '是' : '否';
}
