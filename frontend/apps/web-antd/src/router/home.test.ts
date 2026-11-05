import { describe, expect, it } from 'vitest';

import { getHomePath, getLoginRedirect } from './home';

describe('role landing and login redirects', () => {
  it('opens each role’s own homepage', () => {
    expect(getHomePath('admin')).toBe('/admin/dashboard');
    expect(getHomePath('user')).toBe('/instances');
  });

  it('preserves permitted deep links including query and hash', () => {
    expect(getLoginRedirect(encodeURIComponent('/admin/instances/42?tab=gpu#allocation'), 'admin'))
      .toBe('/admin/instances/42?tab=gpu#allocation');
    expect(getLoginRedirect(encodeURIComponent('/instances?observed_state=running'), 'user'))
      .toBe('/instances?observed_state=running');
    expect(getLoginRedirect(encodeURIComponent('/instances/create?plan=2'), 'user'))
      .toBe('/instances/create?plan=2');
    expect(getLoginRedirect('/instances/create/confirm', 'user')).toBe('/instances/create/confirm');
    expect(getLoginRedirect('/instances/42?tab=gpu', 'user')).toBe('/instances/42?tab=gpu');
    expect(getLoginRedirect('/instances/initialization/43', 'user')).toBe('/instances/initialization/43');
    expect(getLoginRedirect('/account/password', 'user')).toBe('/account/password');
  });

  it.each([
    undefined,
    '/admin/dashboard',
    '/admin/instances/42',
    '/instances/not-an-id',
    '/member-pending',
    '/auth/login',
    '/instances/../admin/dashboard',
    'https://example.com/instances',
    '//example.com/instances',
    '/\\example.com/instances',
    '%E0%A4%A',
  ])('sends a member back home for an invalid or unauthorized redirect: %s', (redirect) => {
    expect(getLoginRedirect(redirect, 'user')).toBe('/instances');
  });

  it('does not send an administrator into the member-only page', () => {
    expect(getLoginRedirect('/instances', 'admin')).toBe('/admin/dashboard');
    expect(getLoginRedirect('/instances/create', 'admin')).toBe('/admin/dashboard');
    expect(getLoginRedirect('/instances/create/confirm', 'admin')).toBe('/admin/dashboard');
    expect(getLoginRedirect('/account/password', 'admin')).toBe('/admin/dashboard');
  });

  it('rejects repeated redirect query values instead of choosing one', () => {
    expect(getLoginRedirect(['/admin/instances/42', '/admin/users'], 'admin'))
      .toBe('/admin/dashboard');
  });
});
