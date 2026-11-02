import type { GPUAllocation } from '#/api/types';

export type GPUAttachmentState =
  | Exclude<GPUAllocation['status'], 'active'>
  | 'attached'
  | 'configuration_missing'
  | 'configuration_unknown';

/** observed 表示 GPU 上下文/设备引用，不代表配置是否已挂载；空闲及停止实例可为 false。 */
export function gpuAttachmentState(allocation: Pick<GPUAllocation, 'configured' | 'status'>): GPUAttachmentState {
  if (allocation.status !== 'active') return allocation.status;
  if (allocation.configured === true) return 'attached';
  return allocation.configured === false ? 'configuration_missing' : 'configuration_unknown';
}
