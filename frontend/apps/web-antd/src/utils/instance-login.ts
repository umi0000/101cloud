import type { CreateLogin, Image, NetworkOption } from '#/api/types';

export interface LoginDraft {
  mode: 'password' | 'ssh-key';
  rootPassword: string;
  rootPasswordConfirmation: string;
  sshPublicKeyText: string;
}

export type LoginSummary =
  | { mode: 'password'; passwordSet: true }
  | { mode: 'ssh-key'; sshKeyCount: number };

export const emptyLoginDraft = (): LoginDraft => ({ mode: 'password', rootPassword: '', rootPasswordConfirmation: '', sshPublicKeyText: '' });

/** Syntax/size feedback only; the server performs authoritative cryptographic validation. */
function canonicalPublicKey(line: string): string | null {
  const match = /^(ssh-ed25519|ssh-rsa|ecdsa-sha2-nistp(?:256|384|521))[\t ]+([A-Za-z0-9+/]+={0,2})(?:[\t ]+[^\p{Cc}]*)?$/u.exec(line.trim());
  if (!match) return null;
  const type = match[1]!;
  let bytes: string;
  try { bytes = atob(match[2]!); } catch { return null; }
  let offset = 0;
  function field(): string | null {
    if (offset + 4 > bytes.length) return null;
    const length = bytes.charCodeAt(offset) * 0x1000000 + bytes.charCodeAt(offset + 1) * 0x10000
      + bytes.charCodeAt(offset + 2) * 0x100 + bytes.charCodeAt(offset + 3);
    offset += 4;
    if (length > bytes.length - offset) return null;
    const result = bytes.slice(offset, offset + length);
    offset += length;
    return result;
  }
  if (field() !== type) return null;
  if (type === 'ssh-ed25519') {
    if (field()?.length !== 32) return null;
  } else if (type === 'ssh-rsa') {
    const exponent = field();
    const modulus = field();
    // SSH mpints are signed; permit only minimally encoded positive integers.
    const positive = (value: string | null): value is string => !!value && value.charCodeAt(0) < 128
      && (value.charCodeAt(0) !== 0 || (value.length > 1 && value.charCodeAt(1) >= 128));
    if (!positive(exponent) || !positive(modulus)) return null;
    const significant = modulus.charCodeAt(0) === 0 ? modulus.slice(1) : modulus;
    const bits = (significant.length - 1) * 8 + Math.floor(Math.log2(significant.charCodeAt(0))) + 1;
    if (bits < 2048) return null;
  } else {
    const curve = type.slice('ecdsa-sha2-'.length);
    const point = field();
    const encodedPoint = field();
    const pointLength = curve === 'nistp256' ? 65 : curve === 'nistp384' ? 97 : 133;
    if (point !== curve || encodedPoint?.length !== pointLength || encodedPoint.charCodeAt(0) !== 4) return null;
  }
  return offset === bytes.length ? `${type} ${btoa(bytes)}` : null;
}

export async function validateLoginDraft(draft: LoginDraft): Promise<{ error: string; login?: CreateLogin; summary?: LoginSummary }> {
  if (draft.mode === 'password') {
    const password = draft.rootPassword;
    if (!password) return { error: '请设置 root 密码。' };
    const length = Array.from(password).length;
    if (length < 8 || length > 128 || /\p{Cc}/u.test(password)) return { error: 'root 密码需为 8–128 个字符，且不能包含控制字符。' };
    if (password !== draft.rootPasswordConfirmation) return { error: '两次输入的 root 密码不一致。' };
    return { error: '', login: { root_password: password }, summary: { mode: 'password', passwordSet: true } };
  }
  if (draft.mode !== 'ssh-key') return { error: '请选择登录方式。' };
  const lines = draft.sshPublicKeyText.split(/\r?\n/u).filter((line) => line.trim());
  if (!lines.length) return { error: '请填写至少一条 SSH 公钥。' };
  if (lines.length > 10) return { error: 'SSH 公钥最多填写 10 条，每行一条。' };
  const keys = new Set<string>();
  for (const [index, line] of lines.entries()) {
    if (new TextEncoder().encode(line).length > 8192) return { error: `第 ${index + 1} 条 SSH 公钥不能超过 8192 字节（含注释）。` };
    const key = canonicalPublicKey(line);
    if (!key) return { error: `第 ${index + 1} 条 SSH 公钥无效，不接受私钥、选项或证书。` };
    keys.add(key);
  }
  return {
    error: '',
    login: { ssh_public_keys: [...keys] },
    summary: { mode: 'ssh-key', sshKeyCount: keys.size },
  };
}

/** Only the selected node/project image variant can authorize credential delivery. */
export function imageSupportsLogin(image: Image, network: NetworkOption): boolean {
  const variant = image.nodes.find((node) => node.node_id === network.node_id && node.project_name === network.project_name);
  return variant ? variant.status === 'ready' && variant.cloud_init : image.nodes.length === 0 && image.cloud_init === true;
}
