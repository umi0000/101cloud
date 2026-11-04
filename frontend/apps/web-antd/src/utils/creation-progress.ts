import type { CreationStageKey, Instance, Task } from '#/api/types';

export const CREATION_STAGES: { key: CreationStageKey; label: string }[] = [
  { key: 'preflight', label: '检查配置与资源' },
  { key: 'native', label: '创建容器' },
  { key: 'gpu', label: '配置 GPU' },
  { key: 'start', label: '启动实例' },
  { key: 'cloud_init', label: '初始化系统与登录' },
  { key: 'script', label: '执行初始化脚本' },
  { key: 'verify', label: '核验实例状态' },
];

export function creationState(task: Task | null): 'active' | 'cancelled' | 'failed' | 'loading' | 'pending' | 'succeeded' | 'unavailable' | 'unknown' {
  if (!task) return 'loading';
  if (task.type !== 'instance.create' || !task.creation_progress) return 'unavailable';
  const progress = task.creation_progress;
  // 已知步骤失败不能覆盖任务整体仍待核对的结果与防重放提示。
  if (task.status === 'unknown') return 'unknown';
  if (task.status === 'failed' || progress.stages.some((stage) => stage.status === 'failed')) return 'failed';
  if (progress.stages.some((stage) => stage.status === 'unknown')) return 'unknown';
  if (task.status === 'cancelled') return 'cancelled';
  if (task.status === 'succeeded') {
    if (progress.initialization_pending) return 'pending';
    const complete = CREATION_STAGES.every(({ key }) => progress.stages.some((stage) =>
      stage.key === key && (stage.status === 'succeeded' || (key !== 'verify' && stage.status === 'skipped'))));
    return complete ? 'succeeded' : 'unavailable';
  }
  return 'active';
}

export function elapsedLabel(start: null | string | undefined, end: number | string) {
  const from = start ? Date.parse(start) : Number.NaN;
  const to = typeof end === 'number' ? end : Date.parse(end);
  if (!Number.isFinite(from) || !Number.isFinite(to) || to < from) return '—';

  const seconds = Math.floor((to - from) / 1000);
  if (seconds < 60) return `${seconds} 秒`;
  const minutes = Math.floor(seconds / 60);
  return minutes < 60 ? `${minutes} 分 ${String(seconds % 60).padStart(2, '0')} 秒` : `${Math.floor(minutes / 60)} 小时 ${minutes % 60} 分`;
}
export function canEnterCreatedInstance(task: Task | null, instance: null | Pick<Instance, 'creation_task_id' | 'id' | 'user_id'>, userId: null | string): boolean {
  return !!userId && creationState(task) === 'succeeded' && !!task?.instance_id
    && instance?.id === task.instance_id && instance.creation_task_id === task.id && instance.user_id === userId;
}
