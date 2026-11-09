import { describe, expect, it } from 'vitest';

import { configFromState, configSummary, stateFromConfig, validateState } from './fields';

describe('Pt policy display and exact editable values', () => {
  it('rounds read-only rates while preserving all six decimal places through form serialization', () => {
    const config = {
      cpu_hourly_pt: '1.250000',
      memory_gib_hourly_pt: '0.049999',
      disk_gib_hourly_pt: '9007199254740993.950000',
    };
    const state = stateFromConfig('pricing', config);
    expect(configSummary('pricing', config)).toContain('CPU 单价 1.3 pt/核·小时');
    expect(configSummary('pricing', config)).toContain('内存单价 0.0 pt/GiB·小时');
    expect(configSummary('pricing', config)).toContain('磁盘单价 9007199254740994.0 pt/GiB·小时');
    expect(validateState('pricing', state)).toEqual({ cross: [], fields: {} });
    expect(configFromState('pricing', state)).toEqual(config);
  });
});

describe('stopped resource billing policy choices', () => {

  it.each(['running_only', 'free'])('accepts explicit %s storage', (mode) => {
    const state = stateFromConfig('credit', { storage_charge_mode: mode });
    expect(validateState('credit', state).fields.storage_charge_mode).toBeUndefined();
  });

  it.each(['retained', 'unknown'])('rejects obsolete or invalid %s storage', (mode) => {
    const state = stateFromConfig('credit', { storage_charge_mode: mode });
    expect(validateState('credit', state).fields.storage_charge_mode).toBeDefined();
  });

});

describe('GPU idle policy configuration', () => {
  const config = {
    enabled: true,
    default_timeout_seconds: 300,
    min_timeout_seconds: 60,
    max_timeout_seconds: 3600,
    allowed_actions: ['release_gpu', 'stop_instance'],
    graceful_stop_timeout_seconds: 30,
    allow_force_stop: false,
  };

  it('validates and serializes the complete idle configuration', () => {
    const state = stateFromConfig('gpu-idle', config);
    expect(validateState('gpu-idle', state)).toEqual({ cross: [], fields: {} });
    expect(configFromState('gpu-idle', state)).toEqual(config);
  });

  it('preserves explicit timeout edits and allowed actions', () => {
    const state = stateFromConfig('gpu-idle', config);
    state.numbers.default_timeout_seconds = 600;
    state.lists.allowed_actions = ['stop_instance'];
    expect(validateState('gpu-idle', state)).toEqual({ cross: [], fields: {} });
    expect(configFromState('gpu-idle', state)).toEqual({
      ...config,
      default_timeout_seconds: 600,
      allowed_actions: ['stop_instance'],
    });
  });

  it.each([30, 7200])('rejects a default timeout outside policy bounds: %s', (timeout) => {
    const state = stateFromConfig('gpu-idle', config);
    state.numbers.default_timeout_seconds = timeout;
    expect(validateState('gpu-idle', state).cross).toEqual(['需满足 最短空闲时长 ≤ 默认空闲时长 ≤ 最长空闲时长']);
  });
});

describe('GPU scheduling configuration', () => {
  it('retains independent utilization thresholds and decimal serialization', () => {
    const config = {
      poll_interval_seconds: 30,
      metrics_max_age_seconds: 60,
      sm_weight: 0.6,
      memory_weight: 0.4,
      source_high_load_pct: 80,
      target_low_load_pct: 20,
      evaluation_window_seconds: 120,
      min_improvement_pct: 10,
      cooldown_seconds: 300,
      rebindings_window_seconds: 3600,
      max_rebindings_per_window: 3,
      max_concurrent_per_node: 1,
      allow_price_increase: false,
    };
    const state = stateFromConfig('gpu-scheduling', config);
    expect(validateState('gpu-scheduling', state)).toEqual({ cross: [], fields: {} });
    expect(configFromState('gpu-scheduling', state)).toEqual(config);
  });
});
