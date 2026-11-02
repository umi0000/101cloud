import { describe, expect, it } from 'vitest';

import { isPrivateIPv4 } from './private-ipv4';

describe('private ipv4 boundaries', () => {
  it.each(['10.0.0.0', '10.255.255.255', '172.16.0.0', '172.31.255.255', '192.168.0.111', '192.168.255.255'])('accepts private address %s', (address) => {
    expect(isPrivateIPv4(address)).toBe(true);
  });

  it.each(['172.15.255.255', '172.32.0.0', '192.167.255.255', '192.169.0.0', '8.8.8.8', '127.0.0.1', '169.254.1.1', '100.64.0.1', '::1', '::ffff:192.168.0.1', '10.1.2.256', '10.01.2.3', '10.1.2', '10.1.2.3/24', '10.1.2.-1', '10.1.2.3 ', ''])('excludes non-private or malformed address %s', (address) => {
    expect(isPrivateIPv4(address)).toBe(false);
  });
});
