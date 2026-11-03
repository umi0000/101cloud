import type { ElasticQuote } from '#/api/types';

import { describe, expect, it } from 'vitest';

import { quoteFitsAvailableCredit, quoteHasValidAmounts, sameOrderTerms, samePolicyVersions } from './order-terms';

function quote(overrides: Partial<ElasticQuote> = {}): ElasticQuote {
  return {
    admissible: true,
    blocking_reasons: [],
    expires_at: '2026-11-03T12:00:00Z',
    initial_window_pt: '1.5',
    plan_revision: 'revision-1',
    policy_versions: { credit: 'credit-1', metrics: 'metrics-1', pricing: 'pricing-1' },
    price_details: {
      cpu_hourly_pt: '1',
      disk_hourly_pt: '0.1',
      gpu_hourly_pt: '0',
      memory_hourly_pt: '0.4',
      total_hourly_pt: '1.5',
    },
    reservation: false,
    ...overrides,
  };
}

describe('confirmed order economics', () => {
  it('compares all component amounts exactly, not only the hourly total', () => {
    const previous = quote();
    for (const field of Object.keys(previous.price_details) as Array<keyof ElasticQuote['price_details']>) {
      expect(sameOrderTerms(previous, quote({
        price_details: { ...previous.price_details, [field]: '0.000001' },
      }))).toBe(false);
    }
    expect(sameOrderTerms(previous, quote({
      price_details: { ...previous.price_details, cpu_hourly_pt: '0.9', disk_hourly_pt: '0.2' },
    }))).toBe(false);
  });

  it('normalizes decimal spelling without losing a micro-point above Number precision', () => {
    const previous = quote({
      initial_window_pt: '001.500000',
      price_details: { ...quote().price_details, total_hourly_pt: '9007199254740993.000001' },
    });
    expect(sameOrderTerms(previous, quote({
      initial_window_pt: '1.5',
      price_details: { ...previous.price_details, total_hourly_pt: '09007199254740993.000001' },
    }))).toBe(true);
    expect(sameOrderTerms(previous, quote({
      price_details: { ...previous.price_details, total_hourly_pt: '9007199254740993.000002' },
    }))).toBe(false);
  });

  it('treats first-window reservation changes and omission as distinct terms', () => {
    expect(sameOrderTerms(quote(), quote({ initial_window_pt: '1.500001' }))).toBe(false);
    expect(sameOrderTerms(quote({ initial_window_pt: undefined }), quote({ initial_window_pt: '0' }))).toBe(false);
    expect(sameOrderTerms(quote({ initial_window_pt: undefined }), quote({ initial_window_pt: undefined }))).toBe(true);
    expect(sameOrderTerms(quote({ initial_window_pt: '0' }), quote({ initial_window_pt: '0.000000' }))).toBe(true);
  });

  it('compares revisions and policy identities independent of map insertion order', () => {
    expect(samePolicyVersions({ pricing: 'p', credit: 'c' }, { credit: 'c', pricing: 'p' })).toBe(true);
    expect(sameOrderTerms(quote(), quote({ policy_versions: { pricing: 'pricing-1', metrics: 'metrics-1', credit: 'credit-1' } }))).toBe(true);
    expect(sameOrderTerms(quote(), quote({ plan_revision: 'revision-2' }))).toBe(false);
    expect(sameOrderTerms(quote(), quote({ policy_versions: { pricing: 'pricing-1', credit: 'credit-2' } }))).toBe(false);
    expect(sameOrderTerms(quote(), quote({ policy_versions: { ...quote().policy_versions, metrics: 'metrics-2' } }))).toBe(false);
    expect(sameOrderTerms(quote(), quote({ policy_versions: { pricing: 'pricing-1' } }))).toBe(false);
    expect(sameOrderTerms(quote(), quote({ policy_versions: { ...quote().policy_versions, 'gpu-pricing': 'gpu-1' } }))).toBe(false);
  });

  it('does not require reconfirmation for expiry renewal or inventory observations alone', () => {
    expect(sameOrderTerms(quote(), quote({ expires_at: '2026-11-03T13:00:00Z' }))).toBe(true);
    expect(sameOrderTerms(quote(), quote({ admissible: false, blocking_reasons: ['inventory_stale'] }))).toBe(true);
  });

  it('requires sufficient current credit without subtracting existing reservations twice', () => {
    expect(quoteFitsAvailableCredit(quote(), '1.500000')).toBe(true);
    expect(quoteFitsAvailableCredit(quote(), '1.499999')).toBe(false);
    expect(quoteFitsAvailableCredit(quote({ initial_window_pt: '0' }), '0')).toBe(true);
    expect(quoteFitsAvailableCredit(quote(), '-0.000001')).toBe(false);
    expect(quoteFitsAvailableCredit(quote({ initial_window_pt: '0' }), '-0.000001')).toBe(false);
    expect(quoteFitsAvailableCredit(quote({ initial_window_pt: undefined }), '100')).toBe(false);
    expect(quoteFitsAvailableCredit(quote({ initial_window_pt: '9007199254740993.000001' }), '9007199254740993.000000')).toBe(false);
  });

  it('fails closed for invalid, negative, or sub-micro-point amounts, including identical invalid quotes', () => {
    for (const value of ['', 'NaN', '1e3', '-0.000001', '1.0000001']) {
      const invalid = quote({ price_details: { ...quote().price_details, total_hourly_pt: value } });
      expect(quoteHasValidAmounts(invalid)).toBe(false);
      expect(sameOrderTerms(invalid, invalid)).toBe(false);
      expect(quoteHasValidAmounts(quote({ initial_window_pt: value }))).toBe(false);
    }
    expect(quoteHasValidAmounts(quote({ initial_window_pt: '0.000001' }))).toBe(true);
    expect(quoteHasValidAmounts(quote({ initial_window_pt: undefined }))).toBe(true);
  });
});
