import type { CreateLogin, Image, NetworkOption } from '#/api/types';

export interface LoginDraft {
  mode: 'password' | 'ssh-key';
  rootPassword: string;
  rootPasswordConfirmation: string;
  sshPublicKeys: string;
}

export interface LoginSummary {
  passwordSet: boolean;
  sshKeys: { fingerprint: string; type: string }[];
}

export const emptyLoginDraft = (): LoginDraft => ({ mode: 'ssh-key', rootPassword: '', rootPasswordConfirmation: '', sshPublicKeys: '' });

function decodeBase64(value: string): Uint8Array | null {
  if (!/^[A-Za-z0-9+/]+={0,2}$/.test(value) || value.length % 4 === 1 || (value.includes('=') && value.length % 4 !== 0)) return null;
  try {
    const decoded = atob(value.padEnd(Math.ceil(value.length / 4) * 4, '='));
    if (btoa(decoded).replace(/=+$/, '') !== value.replace(/=+$/, '')) return null;
    return Uint8Array.from(decoded, (character) => character.charCodeAt(0));
  } catch {
    return null;
  }
}

function readWireString(bytes: Uint8Array, offset: number): { next: number; value: Uint8Array } | null {
  if (offset + 4 > bytes.length) return null;
  const length = new DataView(bytes.buffer, bytes.byteOffset + offset, 4).getUint32(0);
  const start = offset + 4;
  if (!length || start + length > bytes.length) return null;
  return { next: start + length, value: bytes.subarray(start, start + length) };
}

const POINT_LENGTHS: Record<string, number> = { nistp256: 65, nistp384: 97, nistp521: 133 };

const text = (value: Uint8Array) => new TextDecoder('utf-8', { fatal: true }).decode(value);
export async function parsePublicKey(line: string): Promise<{ fingerprint: string; key: string; type: string } | null> {
  if (line.length > 12_000 || /[\u0000-\u0008\u000a-\u001f\u007f-\u009f]/.test(line)) return null;
  const match = /^(ssh-ed25519|ssh-rsa|ecdsa-sha2-nistp(?:256|384|521))[ \t]+([A-Za-z0-9+/]+={0,2})(?:[ \t]+([^\r\n]*))?$/.exec(line.trim());
  if (!match) return null;
  const wire = decodeBase64(match[2]!);
  if (!wire) return null;
  try {
    const typeField = readWireString(wire, 0);
    if (!typeField || text(typeField.value) !== match[1]) return null;
    const offset = typeField.next;
    if (match[1] === 'ssh-rsa') {
      const exponent = readWireString(wire, offset);
      const modulus = exponent && readWireString(wire, exponent.next);
      if (!exponent || !modulus || modulus.next !== wire.length) return null;
      const exponentValue = exponent.value.reduce((value, byte) => value * 256 + byte, 0);
      if (exponent.value.length > 4 || exponentValue < 3 || exponentValue > 0x7fffffff || exponentValue % 2 === 0) return null;
      const value = modulus.value[0] === 0 ? modulus.value.subarray(1) : modulus.value;
      const leading = value[0];
      if (!leading || (modulus.value[0]! & 0x80) !== 0 || (modulus.value[0] === 0 && leading < 0x80)) return null;
      const bits = (value.length - 1) * 8 + (32 - Math.clz32(leading));
      if (bits < 2048) return null;
    } else if (match[1] === 'ssh-ed25519') {
      const key = readWireString(wire, offset);
      if (!key || key.value.length !== 32 || key.next !== wire.length) return null;
    } else {
      const curve = readWireString(wire, offset);
      const point = curve && readWireString(wire, curve.next);
      const expected = match[1].slice('ecdsa-sha2-'.length);
      if (!curve || text(curve.value) !== expected || !point || point.value[0] !== 4 || point.next !== wire.length) return null;
      const size = POINT_LENGTHS[expected];
      if (point.value.length !== size) return null;
    }
    const digest = new Uint8Array(await crypto.subtle.digest('SHA-256', wire.buffer as ArrayBuffer));
    const fingerprint = `SHA256:${btoa(String.fromCharCode(...digest)).replace(/=+$/, '')}`;
    return { fingerprint, key: `${match[1]} ${btoa(String.fromCharCode(...wire))}`, type: match[1] };
  } catch {
    return null;
  }
}

export async function validateLoginDraft(draft: LoginDraft): Promise<{ error: string; login?: CreateLogin; summary?: LoginSummary }> {
  const password = draft.mode === 'password' ? draft.rootPassword : '';
  const sourceLines = draft.mode === 'ssh-key' ? draft.sshPublicKeys.split(/\r?\n/).filter((line) => line.trim()) : [];
  if (!password && sourceLines.length === 0) return { error: '请设置 root 密码或至少添加一条 SSH 公钥。' };
  if (password && (Array.from(password).length < 8 || Array.from(password).length > 128 || /\p{Cc}/u.test(password))) return { error: 'root 密码需为 8–128 个字符，且不能包含控制字符。' };
  if (draft.mode === 'password' && password !== draft.rootPasswordConfirmation) return { error: '两次输入的 root 密码不一致。' };
  if (sourceLines.length > 10) return { error: 'SSH 公钥最多添加 10 条。' };
  const unique = new Map<string, { fingerprint: string; key: string; type: string }>();
  for (const line of sourceLines) {
    if (new TextEncoder().encode(line).length > 8192) return { error: '每条 SSH 公钥不能超过 8192 字节。' };
    const parsed = await parsePublicKey(line);
    if (!parsed) return { error: 'SSH 公钥格式无效。仅支持 OpenSSH 单行 ed25519、RSA（至少 2048 位）或 ECDSA nistp256/384/521 公钥；不能填写私钥或带选项的 authorized_keys 内容。' };
    unique.set(parsed.key.split(' ')[0] + parsed.fingerprint, parsed);
  }
  const sshKeys = [...unique.values()];
  return {
    error: '',
    login: { ...(password ? { root_password: password } : {}), ...(sshKeys.length ? { ssh_public_keys: sshKeys.map(({ key }) => key) } : {}) },
    summary: { passwordSet: !!password, sshKeys: sshKeys.map(({ fingerprint, type }) => ({ fingerprint, type })) },
  };
}

/** Only the selected node/project image variant can authorize credential delivery. */
export function imageSupportsLogin(image: Image, network: NetworkOption): boolean {
  const variant = image.nodes.find((node) => node.node_id === network.node_id && node.project_name === network.project_name);
  return variant ? variant.status === 'ready' && variant.cloud_init : image.nodes.length === 0 && image.cloud_init === true;
}
