/** RFC 1918 私有 IPv4；排除公网、回环、链路本地、IPv6 和不规范地址。 */
export function isPrivateIPv4(address: string): boolean {
  const parts = address.split('.');
  if (parts.length !== 4 || parts.some((part) => !/^(0|[1-9]\d{0,2})$/.test(part) || Number(part) > 255)) return false;
  const first = Number(parts[0]);
  const second = Number(parts[1]);
  return first === 10 || (first === 172 && second >= 16 && second <= 31) || (first === 192 && second === 168);
}
