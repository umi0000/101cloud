import { afterEach, describe, expect, it, vi } from 'vitest';

import { PASSWORD_HINT, passwordError, randomPassword } from './password';

afterEach(() => vi.restoreAllMocks());

describe('panel account password policy', () => {
  it.each(['', 'Abcde12', 'abcdefgh', 'ABCDEFG1', 'abcdefg1', 'Abcdefgh', 'Ａbcdefg1', 'Abcdefg１', `Ab1${'🙂'.repeat(126)}`])('rejects weak or out-of-range input %s', (value) => {
    expect(passwordError(value)).toBe(PASSWORD_HINT);
  });

  it.each(['Abcdefg1', ' Ab1🙂🙂🙂 ', `Ab1${'🙂'.repeat(125)}`, 'Ab1\n\t🙂🙂🙂'])('accepts codepoints and additional characters without trimming %s', (value) => {
    expect(passwordError(value)).toBeNull();
  });

  it('constructs all required categories even when the entropy stream is constant', () => {
    vi.spyOn(crypto, 'getRandomValues').mockImplementation((array) => {
      (array as Uint8Array).fill(0);
      return array;
    });
    for (const length of [8, 24, 128]) {
      const value = randomPassword(length);
      expect([...value]).toHaveLength(length);
      expect(passwordError(value)).toBeNull();
      expect(value).toMatch(/^[A-Za-z0-9_-]+$/u);
    }
  });

  it('rejects biased bytes before sampling a non-power-of-two character class', () => {
    let calls = 0;
    vi.spyOn(crypto, 'getRandomValues').mockImplementation((array) => {
      (array as Uint8Array).fill(calls++ === 0 ? 255 : 0);
      return array;
    });
    expect(passwordError(randomPassword(8))).toBeNull();
    expect(calls).toBeGreaterThan(8);
  });

  it('defaults to 24 characters and rejects unsupported requested lengths', () => {
    expect(randomPassword()).toHaveLength(24);
    for (const length of [0, 7, 129, 8.5, Number.NaN, Number.POSITIVE_INFINITY]) {
      expect(() => randomPassword(length)).toThrow(RangeError);
    }
  });
});
