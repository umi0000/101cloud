import type { Plan, Resources } from '#/api/types';

import { describe, expect, it } from 'vitest';

import {
  bytesToExactGib,
  creationResourceBound,
  creationResourceError,
  estimateRuntimeMinutes,
  exactGibToBytes,
  resourceFromSlider,
  resourceSliderIndex,
  resourceSliderMaximum,
} from './creation-math';

const baseResources: Resources = {
  disk_bytes: '21474836480',
  memory_bytes: '4294967296',
  vcpus: 2,
};

function plan(overrides: Plan['allowed_overrides'] = {}, resources = baseResources): Plan {
  return {
    allowed_overrides: overrides,
    allowed_user_actions: [],
    code: 'elastic-container',
    created_at: '2026-11-02T00:00:00+08:00',
    gpu: { access_mode: 'shared', count: 0, max_hourly_pt: '0', model: null },
    gpu_policy: {
      idle: { action: 'detach_gpu', enabled: false, timeout_seconds: 0 },
      rebalance: { allow_live: false, enabled: false, max_hourly_pt: '0' },
    },
    gpu_user_permissions: {},
    id: '1',
    image_ids: ['1'],
    init_template_version_id: null,
    instance_type: 'container',
    name: 'Elastic',
    network_option_ids: ['1'],
    node_ids: ['1'],
    policy_versions: { pricing: 'pricing-1', credit: 'credit-1', metrics: 'metrics-1' },
    pricing_version_id: null,
    resources,
    revision: '1',
    status: 'active',
    updated_at: '2026-11-02T00:00:00+08:00',
    version: '1',
  };
}

describe('exact creation resources', () => {
  it('retains the full GiB fraction for a single byte and values above the safe integer boundary', () => {
    expect(exactGibToBytes('0.000000000931322574615478515625')).toBe('1');
    for (const bytes of ['1', '1073741825', '9007199254740993', '18446744073709551615']) {
      expect(exactGibToBytes(bytesToExactGib(bytes))).toBe(bytes);
    }
  });

  it('rejects fractions of a byte rather than silently rounding the payload', () => {
    expect(exactGibToBytes('0.1')).toBeNull();
    expect(exactGibToBytes('1.0000000000000000000000000000001')).toBeNull();
    expect(exactGibToBytes('1.250000000000000000000000000000000')).toBe('1342177280');
    for (const invalid of ['', '-1', '1.', '1e3', 'NaN']) expect(exactGibToBytes(invalid)).toBeNull();
  });

  it('anchors steps at the plan minimum and does not treat an unaligned maximum as legal', () => {
    const selected = plan({ vcpus: { max: '10', min: '2', step: '3' } });
    for (const vcpus of [2, 5, 8]) {
      expect(creationResourceError(selected, { ...baseResources, vcpus })).toBeNull();
    }
    for (const vcpus of [1, 3, 9, 10, 11]) {
      expect(creationResourceError(selected, { ...baseResources, vcpus })).not.toBeNull();
    }
    const bound = creationResourceBound(selected, 'vcpus');
    expect(bound).not.toBeNull();
    if (!bound) throw new Error('Expected resource bound');
    expect(resourceSliderMaximum(bound)).toBe(2);
    expect(resourceFromSlider(bound, 2)).toBe(8n);
    expect(resourceFromSlider(bound, 3)).toBeNull();
    expect(resourceFromSlider(bound, 1.5)).toBeNull();
  });

  it('keeps resources fixed when the plan does not allow an override', () => {
    const selected = plan();
    expect(creationResourceError(selected, baseResources)).toBeNull();
    expect(creationResourceError(selected, { ...baseResources, memory_bytes: '4294967297' })).not.toBeNull();
    expect(creationResourceError(selected, { ...baseResources, vcpus: 3 })).not.toBeNull();
  });

  it('preserves byte-level step alignment beyond Number.MAX_SAFE_INTEGER', () => {
    const selected = plan({
      memory_bytes: { max: '9007199254741002', min: '9007199254740993', step: '3' },
    }, { ...baseResources, memory_bytes: '9007199254740993' });
    for (const memory_bytes of ['9007199254740993', '9007199254740996', '9007199254741002']) {
      expect(creationResourceError(selected, { ...baseResources, memory_bytes })).toBeNull();
    }
    for (const memory_bytes of ['9007199254740992', '9007199254740994', '9007199254741003']) {
      expect(creationResourceError(selected, { ...baseResources, memory_bytes })).not.toBeNull();
    }
    const bound = creationResourceBound(selected, 'memory_bytes');
    if (!bound) throw new Error('Expected resource bound');
    expect(resourceSliderMaximum(bound)).toBe(3);
    expect(resourceFromSlider(bound, 1)).toBe(9007199254740996n);
    expect(resourceSliderIndex(bound, 9007199254740996n)).toBe(1);
    expect(resourceSliderIndex(bound, 9007199254740994n)).toBeNull();
  });

  it('disables only the slider for an unsafe step count while preserving exact legal input', () => {
    const selected = plan({ disk_bytes: { max: '18446744073709551615', min: '1', step: '1' } });
    const bound = creationResourceBound(selected, 'disk_bytes');
    if (!bound) throw new Error('Expected resource bound');
    expect(resourceSliderMaximum(bound)).toBeNull();
    expect(resourceSliderIndex(bound, 18446744073709551615n)).toBeNull();
    expect(creationResourceError(selected, { ...baseResources, disk_bytes: '18446744073709551615' })).toBeNull();
  });

  it('rejects noninteger, nonpositive, and out-of-range payload resources', () => {
    const selected = plan({
      disk_bytes: { max: '18446744073709551615', min: '1', step: '1' },
      vcpus: { max: '4294967295', min: '1', step: '1' },
    });
    for (const disk_bytes of ['0', '-1', '1.5', '18446744073709551616']) {
      expect(creationResourceError(selected, { ...baseResources, disk_bytes })).not.toBeNull();
    }
    for (const vcpus of [0, -1, 1.5, Number.NaN, 4294967296, Number.MAX_SAFE_INTEGER + 1]) {
      expect(creationResourceError(selected, { ...baseResources, vcpus })).not.toBeNull();
    }
  });
});

describe('runtime minute estimates', () => {
  it.each([
    ['1', '7', 8n],
    ['0.000001', '0.000060', 1n],
    ['0.000001', '0.000061', 0n],
    ['0', '1', 0n],
    ['10.5', '3', 210n],
    ['9007199254740993', '60', 9007199254740993n],
  ])('floors %s pt at %s pt/hour to whole minutes', (available, hourly, expected) => {
    expect(estimateRuntimeMinutes(available, hourly)).toBe(expected);
  });

  it.each([
    ['1', '0'],
    ['0', '0'],
    ['-1', '1'],
    ['1', '-1'],
    ['', '1'],
    ['1', 'invalid'],
    ['1.0000001', '1'],
  ])('does not invent a duration for invalid or free-rate input (%s, %s)', (available, hourly) => {
    expect(estimateRuntimeMinutes(available, hourly)).toBeNull();
  });
});
