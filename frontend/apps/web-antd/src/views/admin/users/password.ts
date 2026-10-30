/** 后端规则：12–128 个字符（按 Unicode 字符计）。 */
export const PASSWORD_MIN = 12;
export const PASSWORD_MAX = 128;

const ALPHABET = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_';

/** 24 位 URL 安全随机密码。字母表 64 个字符，取字节低 6 位无偏。 */
export function randomPassword(length = 24): string {
  const bytes = crypto.getRandomValues(new Uint8Array(length));
  return Array.from(bytes, (b) => ALPHABET[b & 63]).join('');
}

export function passwordError(value: string): null | string {
  const n = [...value].length;
  if (n < PASSWORD_MIN || n > PASSWORD_MAX) return `密码需 ${PASSWORD_MIN}–${PASSWORD_MAX} 个字符`;
  return null;
}
