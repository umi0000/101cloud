import type { Image, NetworkOption } from '#/api/types';

import { Buffer } from 'node:buffer';
import { generateKeyPairSync, webcrypto } from 'node:crypto';

import { afterAll, beforeAll, describe, expect, it, vi } from 'vitest';

import { emptyLoginDraft, imageSupportsLogin, parsePublicKey, validateLoginDraft } from './instance-login';

function wire(...fields: Uint8Array[]): string {
  return Buffer.concat(fields.map((field) => {
    const length = Buffer.alloc(4);
    length.writeUInt32BE(field.length);
    return Buffer.concat([length, field]);
  })).toString('base64');
}

function publicKey(type: 'ec' | 'ed25519' | 'rsa', size = 2048): string {
  if (type === 'ed25519') {
    const key = generateKeyPairSync('ed25519').publicKey.export({ format: 'jwk' });
    return `ssh-ed25519 ${wire(Buffer.from('ssh-ed25519'), Buffer.from(key.x!, 'base64url'))}`;
  }
  if (type === 'rsa') {
    const key = generateKeyPairSync('rsa', { modulusLength: size }).publicKey.export({ format: 'jwk' });
    const modulus = Buffer.from(key.n!, 'base64url');
    return `ssh-rsa ${wire(Buffer.from('ssh-rsa'), Buffer.from(key.e!, 'base64url'), Buffer.concat([Buffer.from([0]), modulus]))}`;
  }
  const bits = size === 384 || size === 521 ? size : 256;
  const curves: Record<number, string> = { 256: 'prime256v1', 384: 'secp384r1', 521: 'secp521r1' };
  const key = generateKeyPairSync('ec', { namedCurve: curves[bits]! }).publicKey.export({ format: 'jwk' });
  const typeName = `ecdsa-sha2-nistp${bits}`;
  return `${typeName} ${wire(Buffer.from(typeName), Buffer.from(`nistp${bits}`), Buffer.concat([Buffer.from([4]), Buffer.from(key.x!, 'base64url'), Buffer.from(key.y!, 'base64url')]))}`;
}

beforeAll(() => vi.stubGlobal('crypto', webcrypto));
afterAll(() => vi.unstubAllGlobals());

const passwordDraft = (password: string) => ({ ...emptyLoginDraft(), mode: 'password' as const, rootPassword: password, rootPasswordConfirmation: password });

describe('instance root login validation', () => {
  it('requires credentials and confirms passwords without trimming spaces', async () => {
    expect((await validateLoginDraft(emptyLoginDraft())).error).not.toBe('');
    const value = '  独立🙂密码  ';
    const result = await validateLoginDraft(passwordDraft(value));
    expect(result.error).toBe('');
    expect(result.login).toEqual({ root_password: value });
    expect(result.summary).toEqual({ passwordSet: true, sshKeys: [] });
    expect((await validateLoginDraft({ ...passwordDraft(value), rootPasswordConfirmation: value.trim() })).error).not.toBe('');
  });

  it('counts Unicode codepoints and rejects every control-character range', async () => {
    expect((await validateLoginDraft(passwordDraft('🙂'.repeat(8)))).error).toBe('');
    expect((await validateLoginDraft(passwordDraft('🙂'.repeat(128)))).error).toBe('');
    for (const value of ['🙂'.repeat(7), '🙂'.repeat(129), 'password\n', 'password\u0000', 'password\u007f', 'password\u0085', 'password\u009f']) {
      expect((await validateLoginDraft(passwordDraft(value))).error).not.toBe('');
    }
  });

  it('parses actual ed25519/RSA/ECDSA keys and deduplicates key material, not comments', async () => {
    const ed = publicKey('ed25519');
    const result = await validateLoginDraft({ ...emptyLoginDraft(), sshPublicKeys: `${ed} first\r\n${ed} second\n${publicKey('rsa')}\n${publicKey('ec')}` });
    expect(result.error).toBe('');
    expect(result.login?.ssh_public_keys).toHaveLength(3);
    expect(result.summary?.sshKeys.map(({ type }) => type)).toEqual(['ssh-ed25519', 'ssh-rsa', 'ecdsa-sha2-nistp256']);
    for (const key of result.summary!.sshKeys) expect(key.fingerprint).toMatch(/^SHA256:[A-Za-z0-9+/]{43}$/);
    expect(JSON.stringify(result.summary)).not.toContain(ed);
  });

  it.each([256, 384, 521])('accepts actual ECDSA nistp%s keys', async (size) => {
    const key = await parsePublicKey(publicKey('ec', size));
    expect(key?.type).toBe(`ecdsa-sha2-nistp${size}`);
  });

  it('normalizes whitespace, comments and optional base64 padding', async () => {
    const source = publicKey('ec', 384);
    const parsed = await parsePublicKey(`  ${source.replace(' ', '\t').replace(/=+$/, '')} workstation  `);
    expect(parsed?.key).toBe(source);
    expect(parsed?.fingerprint).toBe((await parsePublicKey(source))?.fingerprint);
  });

  it('rejects private keys, options, scripts, weak RSA and malformed wire data', async () => {
    const ed = publicKey('ed25519');
    for (const value of ['-----BEGIN OPENSSH PRIVATE KEY-----', `command="id" ${ed}`, `from="127.0.0.1" ${ed}`, '#!/bin/sh\nid', publicKey('rsa', 1024), 'ssh-ed25519 AAAA', ed.replace('ssh-ed25519 ', 'ssh-rsa ')]) {
      expect((await validateLoginDraft({ ...emptyLoginDraft(), sshPublicKeys: value })).error).not.toBe('');
    }
    const decoded = Buffer.from(ed.split(' ')[1]!, 'base64');
    expect(await parsePublicKey(`ssh-ed25519 ${Buffer.concat([decoded, Buffer.from([0])]).toString('base64')}`)).toBeNull();
  });

  it('enforces raw line byte/count limits before deduplication', async () => {
    const ed = publicKey('ed25519');
    const keyDraft = (sshPublicKeys: string) => ({ ...emptyLoginDraft(), sshPublicKeys });
    expect((await validateLoginDraft(keyDraft(Array.from({ length: 11 }, () => ed).join('\n')))).error).not.toBe('');
    expect((await validateLoginDraft(keyDraft(`${ed} ${'中'.repeat(3000)}`))).error).not.toBe('');
    const max = `${ed} ${'a'.repeat(8192 - ed.length - 1)}`;
    expect((await validateLoginDraft(keyDraft(max))).error).toBe('');
    expect((await validateLoginDraft(keyDraft(`${max}a`))).error).not.toBe('');
  });

  it('sends only the selected mode even if an inactive field contains old input', async () => {
    const result = await validateLoginDraft({ ...passwordDraft('password1'), sshPublicKeys: publicKey('ed25519') });
    expect(result.login).toEqual({ root_password: 'password1' });
    const keys = await validateLoginDraft({ ...passwordDraft('password1'), mode: 'ssh-key', sshPublicKeys: publicKey('ed25519') });
    expect(keys.login?.root_password).toBeUndefined();
    expect(keys.summary?.passwordSet).toBe(false);
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
