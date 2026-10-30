import type { ID, PolicyVersion } from '#/api/types';

/** 额度政策版本 id → `code v{version}`；列表中找不到时显示原始 id。 */
export function policyLabel(versions: PolicyVersion[], id: ID | null | undefined): string {
  if (!id) return '—';
  const v = versions.find((x) => x.id === id);
  return v ? `${v.code} v${v.version}` : `#${id}`;
}

/** 后端 validateBindingVersion：仅 active 且 effective_at 已到的版本可绑定到账户。 */
export function isEffective(v: PolicyVersion, now = Date.now()): boolean {
  return v.status === 'active' && !!v.effective_at && Date.parse(v.effective_at) <= now;
}
