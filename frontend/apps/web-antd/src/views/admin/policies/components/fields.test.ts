import { describe, expect, it } from 'vitest';

import { stateFromConfig, validateState } from './fields';

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
