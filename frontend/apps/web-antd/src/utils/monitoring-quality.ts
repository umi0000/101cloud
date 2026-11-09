import type { MetricQuality } from '#/api/types';

// Display only. Never use these helpers to authorize GPU operations or mutate API quality.
export function hasSharedControlReferences(quality: MetricQuality): boolean {
  return !quality.error_code
    && (quality.reference_scan_quality === 'partial' || quality.reference_scan_quality === 'complete')
    && quality.reasons.includes('gpu_shared_control_references');
}

export function monitoringReasons(quality: MetricQuality): string[] {
  const reasons = new Set(quality.reasons);
  if (quality.error_code) reasons.add(quality.error_code);
  if (hasSharedControlReferences(quality)) reasons.delete('gpu_shared_control_references');
  return [...reasons];
}

export function sharedControlOnly(quality: MetricQuality): boolean {
  return hasSharedControlReferences(quality)
    && quality.reasons.every((reason) => reason === 'gpu_shared_control_references' || reason === 'gpu_utilization_no_samples');
}
