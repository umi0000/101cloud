import type { CustomConfig } from '#/api/types';

import { formatPt } from '#/utils/format';

/** 用于比较申请与批准配置的规范化字段值（键即高亮用的字段名）。 */
function configRaw(c: CustomConfig): Record<string, string> {
  return {
    access_mode: c.gpu.access_mode,
    disk_bytes: c.resources.disk_bytes,
    gpu_count: String(c.gpu.count),
    gpu_model: c.gpu.model ?? '',
    image_id: c.image_id,
    init_parameters: JSON.stringify(c.init_parameters ?? {}),
    init_template_version_id: c.init_template_version_id ?? '',
    instance_type: c.instance_type,
    max_hourly_pt: formatPt(c.gpu.max_hourly_pt),
    memory_bytes: c.resources.memory_bytes,
    network_option_id: c.network_option_id,
    vcpus: String(c.resources.vcpus),
    zone_id: c.zone_id,
  };
}

/** 两份配置中取值不同的字段名。 */
export function diffKeys(a: CustomConfig, b: CustomConfig): Set<string> {
  const ra = configRaw(a);
  const rb = configRaw(b);
  return new Set(Object.keys(ra).filter((k) => ra[k] !== rb[k]));
}

/** 高亮差异字段的样式。 */
export const DIFF_CLASS = 'rounded-sm bg-amber-50 ring-1 ring-amber-300 dark:bg-amber-950';
