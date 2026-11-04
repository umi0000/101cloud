import type { Image, NetworkOption } from '#/api/types';

import { describe, expect, it } from 'vitest';

import { emptyLoginDraft, imageSupportsLogin, validateLoginDraft } from './instance-login';

const passwordDraft = (password: string) => ({ ...emptyLoginDraft(), rootPassword: password, rootPasswordConfirmation: password });

function wireKey(type: string, ...fields: string[]) {
  const blob = [type, ...fields].map((field) => String.fromCharCode(field.length >>> 24, (field.length >>> 16) & 255, (field.length >>> 8) & 255, field.length & 255) + field).join('');
  return `${type} ${btoa(blob)}`;
}
const ed25519 = wireKey('ssh-ed25519', String.fromCharCode(...'d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a'.match(/../gu)!.map((byte) => Number.parseInt(byte, 16))));
const keyDraft = (sshPublicKeyText: string) => ({ ...emptyLoginDraft(), mode: 'ssh-key' as const, sshPublicKeyText });

describe('instance root login validation', () => {
  it('requires credentials and confirms passwords without trimming spaces', async () => {
    expect((await validateLoginDraft(emptyLoginDraft())).error).not.toBe('');
    const value = '  独立🙂密码  ';
    const result = await validateLoginDraft(passwordDraft(value));
    expect(result.error).toBe('');
    expect(result.login).toEqual({ root_password: value });
    expect(result.summary).toEqual({ mode: 'password', passwordSet: true });
    expect((await validateLoginDraft({ ...passwordDraft(value), rootPasswordConfirmation: value.trim() })).error).not.toBe('');
  });

  it('counts Unicode codepoints and rejects every control-character range', async () => {
    expect((await validateLoginDraft(passwordDraft('🙂'.repeat(8)))).error).toBe('');
    expect((await validateLoginDraft(passwordDraft('🙂'.repeat(128)))).error).toBe('');
    for (const value of ['🙂'.repeat(7), '🙂'.repeat(129), 'password\n', 'password\u0000', 'password\u007f', 'password\u0085', 'password\u009f']) {
      expect((await validateLoginDraft(passwordDraft(value))).error).not.toBe('');
    }
  });

  it('requires a matching confirmation and exposes only selected-mode metadata', async () => {
    const password = 'root-password🙂';
    const result = await validateLoginDraft(passwordDraft(password));
    expect(Object.keys(result.login!)).toEqual(['root_password']);
    expect(JSON.stringify(result.summary)).not.toContain(password);
    expect(result.summary).toEqual({ mode: 'password', passwordSet: true });
    expect((await validateLoginDraft({ ...passwordDraft(password), rootPasswordConfirmation: '' })).error).not.toBe('');
    expect((await validateLoginDraft({ ...passwordDraft(''), rootPasswordConfirmation: password })).error).not.toBe('');
  });

  it('requires keys only in SSH mode, strips comments and deduplicates canonical keys', async () => {
    expect((await validateLoginDraft(keyDraft(' \n\r\n '))).error).not.toBe('');
    const result = await validateLoginDraft(keyDraft(`  ${ed25519} laptop\r\n\n${ed25519} other comment`));
    expect(result.error).toBe('');
    expect(result.login).toEqual({ ssh_public_keys: [ed25519] });
    expect(result.summary).toEqual({ mode: 'ssh-key', sshKeyCount: 1 });
    expect(JSON.stringify(result.summary)).not.toContain(ed25519);
  });

  it('ignores inactive fields and never submits both credentials', async () => {
    const password = 'root-password🙂';
    expect((await validateLoginDraft({ ...passwordDraft(password), sshPublicKeyText: 'invalid inactive key' })).login).toEqual({ root_password: password });
    expect((await validateLoginDraft({ ...keyDraft(ed25519), rootPassword: 'invalid', rootPasswordConfirmation: 'different' })).login).toEqual({ ssh_public_keys: [ed25519] });
    expect(emptyLoginDraft().mode).toBe('password');
  });

  it('checks supported key wire shapes while leaving cryptographic validity to the server', async () => {
    const rsa = wireKey('ssh-rsa', '\x01\x00\x01', `\0\x80${'\x01'.repeat(255)}`);
    for (const key of [ed25519, rsa, ...(['nistp256', 'nistp384', 'nistp521'] as const).map((curve) => wireKey(`ecdsa-sha2-${curve}`, curve, `\x04${'\x01'.repeat(curve === 'nistp256' ? 64 : curve === 'nistp384' ? 96 : 132)}`))]) {
      expect((await validateLoginDraft(keyDraft(key))).error).toBe('');
    }
  });

  it('rejects private keys, options, certificates, mismatched/truncated/trailing blobs, weak RSA and unsupported types without echoing input', async () => {
    const invalid = [
      '-----BEGIN OPENSSH PRIVATE KEY-----', `command="false" ${ed25519}`, `no-agent-forwarding ${ed25519}`,
      ed25519.replace('ssh-ed25519 ', 'ssh-ed25519-cert-v01@openssh.com '), 'garbage-sensitive-key', 'ssh-ed25519 not_base64',
      ed25519.replace('ssh-ed25519 ', 'ssh-rsa '), wireKey('ssh-ed25519', 'short'), wireKey('ssh-ed25519', 'a'.repeat(32), 'extra'),
      wireKey('ssh-rsa', '\x01\x00\x01', `\0\x80${'\x01'.repeat(127)}`), wireKey('ssh-dss', 'unsupported'),
      wireKey('ecdsa-sha2-nistp256', 'nistp384', `\x04${'x'.repeat(64)}`), `${ed25519} private\u0000comment`,
    ];
    for (const key of invalid) {
      const result = await validateLoginDraft(keyDraft(key));
      expect(result.error).not.toBe('');
      expect(result.login).toBeUndefined();
      expect(result.error).not.toContain(key);
    }
  });

  it('enforces ten entries before deduplication and 8192 UTF-8 bytes per entry including comments', async () => {
    expect((await validateLoginDraft(keyDraft(Array.from({ length: 10 }, () => ed25519).join('\n')))).summary).toEqual({ mode: 'ssh-key', sshKeyCount: 1 });
    expect((await validateLoginDraft(keyDraft(Array.from({ length: 11 }, () => ed25519).join('\n')))).error).not.toBe('');
    const remaining = 8192 - new TextEncoder().encode(`${ed25519} `).length;
    const exact = `${ed25519} ${'密'.repeat(Math.floor(remaining / 3))}${'x'.repeat(remaining % 3)}`;
    expect((await validateLoginDraft(keyDraft(exact))).error).toBe('');
    expect((await validateLoginDraft(keyDraft(`${exact}x`))).error).not.toBe('');
  });
});

describe('node/project cloud-init boundary', () => {
  const network = { node_id: 'node-a', project_name: 'default' } as NetworkOption;
  const variant = { node_id: 'node-a', project_name: 'default', cloud_init: true, status: 'ready' };
  const image = (nodes: unknown[], cloud_init = true) => ({ nodes, cloud_init }) as Image;

  it('uses the selected variant instead of another node or logical image flag', () => {
    expect(imageSupportsLogin(image([variant], false), network)).toBe(true);
    expect(imageSupportsLogin(image([{ ...variant, cloud_init: false }]), network)).toBe(false);
    expect(imageSupportsLogin(image([{ ...variant, status: 'pending' }]), network)).toBe(false);
    expect(imageSupportsLogin(image([{ ...variant, node_id: 'node-b' }]), network)).toBe(false);
    expect(imageSupportsLogin(image([{ ...variant, project_name: 'other' }]), network)).toBe(false);
    expect(imageSupportsLogin(image([], true), network)).toBe(true);
    expect(imageSupportsLogin(image([], false), network)).toBe(false);
  });
});
