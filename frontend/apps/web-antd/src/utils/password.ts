/** 面板账户密码按 Unicode 码点计数；不裁剪或规范化输入。 */
export const PASSWORD_MIN = 8;
export const PASSWORD_MAX = 128;
export const PASSWORD_HINT = `密码需 ${PASSWORD_MIN}–${PASSWORD_MAX} 个 Unicode 字符，且包含大写字母（A–Z）、小写字母（a–z）和数字（0–9）`;

const UPPERCASE = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ';
const LOWERCASE = 'abcdefghijklmnopqrstuvwxyz';
const DIGITS = '0123456789';
const ALPHABET = `${UPPERCASE}${LOWERCASE}${DIGITS}-_`;

/** 拒绝超出整倍数区间的字节，避免取模偏差。 */
function randomIndex(size: number): number {
  const byte = new Uint8Array(1);
  const limit = 256 - (256 % size);
  do { crypto.getRandomValues(byte); } while (byte[0]! >= limit);
  return byte[0]! % size;
}

/** 默认 24 位 URL 安全密码；先保证三类字符，再以无偏 Fisher–Yates 打乱位置。 */
export function randomPassword(length = 24): string {
  if (!Number.isInteger(length) || length < PASSWORD_MIN || length > PASSWORD_MAX) {
    throw new RangeError(`密码长度需为 ${PASSWORD_MIN}–${PASSWORD_MAX} 的整数`);
  }
  const characters = [UPPERCASE[randomIndex(UPPERCASE.length)]!, LOWERCASE[randomIndex(LOWERCASE.length)]!, DIGITS[randomIndex(DIGITS.length)]!];
  while (characters.length < length) characters.push(ALPHABET[randomIndex(ALPHABET.length)]!);
  for (let i = characters.length - 1; i > 0; i--) {
    const j = randomIndex(i + 1);
    [characters[i], characters[j]] = [characters[j]!, characters[i]!];
  }
  return characters.join('');
}

export function passwordError(value: string): null | string {
  const n = [...value].length;
  if (n < PASSWORD_MIN || n > PASSWORD_MAX || !/[A-Z]/u.test(value) || !/[a-z]/u.test(value) || !/[0-9]/u.test(value)) return PASSWORD_HINT;
  return null;
}
