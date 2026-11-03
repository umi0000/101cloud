import { describe, expect, it } from 'vitest';

import { creationResultUncertain } from './order-intent';

describe('creation intent outcomes', () => {
  it.each([
    { label: 'lost connection', status: 0, retrying: false, uncertain: true },
    { label: 'server failure', status: 500, retrying: false, uncertain: true },
    { label: 'request timeout', status: 408, retrying: false, uncertain: true },
    { label: 'unknown transport failure', status: undefined, retrying: false, uncertain: true },
    { label: 'first unauthorized request', status: 401, retrying: false, uncertain: false },
    { label: 'unauthorized retry of an uncertain request', status: 401, retrying: true, uncertain: true },
    { label: 'first forbidden request', status: 403, retrying: false, uncertain: false },
    { label: 'forbidden retry of an uncertain request', status: 403, retrying: true, uncertain: true },
    { label: 'validated rejection', status: 422, retrying: true, uncertain: false },
    { label: 'configuration conflict', status: 409, retrying: true, uncertain: false },
  ])('classifies $label without discarding an unknown original result', ({ status, retrying, uncertain }) => {
    expect(creationResultUncertain(status, retrying)).toBe(uncertain);
  });
});
