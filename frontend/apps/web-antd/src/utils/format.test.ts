import { describe, expect, it } from 'vitest';

import { formatPt, PT_PATTERN, ptToMicros, SIGNED_PT_PATTERN } from './format';

describe('formatPt display rounding', () => {
  it.each([null, undefined, ''])('retains the missing-value marker for %s', (value) => {
    expect(formatPt(value)).toBe('—');
  });

  it.each([
    ['0', '0.0'],
    ['1', '1.0'],
    ['-1', '-1.0'],
    ['5.8', '5.8'],
    ['5.800000', '5.8'],
    ['0005.800000', '5.8'],
    ['1.234567', '1.2'],
    ['1.249999', '1.2'],
    ['1.250000', '1.3'],
    ['1.250001', '1.3'],
    ['-1.249999', '-1.2'],
    ['-1.250000', '-1.3'],
    ['-1.250001', '-1.3'],
    ['9.949999', '9.9'],
    ['9.950000', '10.0'],
    ['-9.950000', '-10.0'],
    ['0.049999', '0.0'],
    ['0.050000', '0.1'],
    ['0.050001', '0.1'],
    ['-0.049999', '0.0'],
    ['-0.050000', '-0.1'],
    ['-0.050001', '-0.1'],
    ['0.000001', '0.0'],
    ['-0.000001', '0.0'],
    ['-0', '0.0'],
    ['-0.000000', '0.0'],
    ['000.000000', '0.0'],
  ])('formats %s as %s with exact half-up rounding', (value, expected) => {
    expect(formatPt(value)).toBe(expected);
  });

  it.each([
    ['9007199254740993', '9007199254740993.0'],
    ['9007199254740993.149999', '9007199254740993.1'],
    ['9007199254740993.150000', '9007199254740993.2'],
    ['-9007199254740993.150000', '-9007199254740993.2'],
    ['99999999999999999999999999999999999999.950000', '100000000000000000000000000000000000000.0'],
    ['-99999999999999999999999999999999999999.950000', '-100000000000000000000000000000000000000.0'],
  ])('preserves arbitrary-precision integer digits for %s', (value, expected) => {
    expect(formatPt(value)).toBe(expected);
  });

  it('does not change exact micro-Pt conversion or six-decimal input validation', () => {
    const value = '9007199254740993.149999';
    expect(formatPt(value)).toBe('9007199254740993.1');
    expect(ptToMicros(value)).toBe(9_007_199_254_740_993_149_999n);
    expect(ptToMicros('-0.049999')).toBe(-49_999n);
    expect(ptToMicros('0.000001')).toBe(1n);
    expect(PT_PATTERN.test('1.234567')).toBe(true);
    expect(SIGNED_PT_PATTERN.test('-1.234567')).toBe(true);
    expect(PT_PATTERN.test('1.2345678')).toBe(false);
    expect(SIGNED_PT_PATTERN.test('-1.2345678')).toBe(false);
    expect(ptToMicros('1.2345678')).toBeNull();
  });
});
