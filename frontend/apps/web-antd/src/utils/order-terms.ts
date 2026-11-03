import type { ElasticQuote, PolicyVersions } from '#/api/types';

import { ptToMicros } from './format';

const PRICE_FIELDS = [
  'cpu_hourly_pt',
  'memory_hourly_pt',
  'disk_hourly_pt',
  'gpu_hourly_pt',
  'total_hourly_pt',
] as const;

function amount(value: string): bigint | null {
  const parsed = ptToMicros(value);
  return parsed !== null && parsed >= 0n ? parsed : null;
}

export function samePolicyVersions(left: PolicyVersions, right: PolicyVersions): boolean {
  const keys = new Set([...Object.keys(left), ...Object.keys(right)]);
  return [...keys].every((key) => left[key as keyof PolicyVersions] === right[key as keyof PolicyVersions]);
}

/** Reject malformed economics rather than treating two invalid amounts as equal. */
export function quoteHasValidAmounts(quote: ElasticQuote): boolean {
  return PRICE_FIELDS.every((field) => amount(quote.price_details[field]) !== null)
    && (quote.initial_window_pt === undefined || amount(quote.initial_window_pt) !== null);
}

/** The displayed balance already excludes reservations; compare it once to the new window. */
export function quoteFitsAvailableCredit(quote: ElasticQuote, availablePt: string): boolean {
  const available = amount(availablePt);
  const required = quote.initial_window_pt === undefined ? null : amount(quote.initial_window_pt);
  return available !== null && required !== null && available >= required;
}

/** Expiry and inventory observations are not economic terms being confirmed. */
export function sameOrderTerms(left: ElasticQuote, right: ElasticQuote): boolean {
  if (!quoteHasValidAmounts(left) || !quoteHasValidAmounts(right)) return false;
  if (left.plan_revision !== right.plan_revision || !samePolicyVersions(left.policy_versions, right.policy_versions)) return false;
  if (!PRICE_FIELDS.every((field) => amount(left.price_details[field]) === amount(right.price_details[field]))) return false;
  if (left.initial_window_pt === undefined || right.initial_window_pt === undefined) {
    return left.initial_window_pt === right.initial_window_pt;
  }
  return amount(left.initial_window_pt) === amount(right.initial_window_pt);
}
