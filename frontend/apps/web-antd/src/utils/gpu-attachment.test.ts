import { describe, expect, it } from 'vitest';

import { gpuAttachmentState } from './gpu-attachment';

describe('gpu attachment and usage are independent', () => {
  it.each([true, false, null])('keeps a configured active GPU attached when usage observation is %s', (observed) => {
    const allocation = { configured: true, observed, status: 'active' as const };
    expect(gpuAttachmentState(allocation)).toBe('attached');
  });

  it.each([true, false, null])('does not use process observations to hide a missing GPU configuration: %s', (observed) => {
    const allocation = { configured: false, observed, status: 'active' as const };
    expect(gpuAttachmentState(allocation)).toBe('configuration_missing');
  });

  it('does not infer configuration from a positive usage observation', () => {
    const allocation = { configured: null, observed: true, status: 'active' as const };
    expect(gpuAttachmentState(allocation)).toBe('configuration_unknown');
  });

  it.each(['reserved', 'attaching', 'releasing', 'failed', 'unknown', 'released', 'cancelled'] as const)(
    'does not turn a %s allocation into attached based on an old configuration',
    (status) => {
      expect(gpuAttachmentState({ configured: true, status })).toBe(status);
    },
  );
});
