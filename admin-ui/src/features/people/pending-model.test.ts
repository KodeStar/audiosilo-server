import type { AdminUser, OIDCPendingSignIn } from '@/api/types';
import { admin, sam } from '@/test/fixtures';
import {
  groupsShown,
  identityProvider,
  linkCandidates,
  pendingName,
  reasonKey,
  signInProvider,
  suggestedAccount,
} from './pending-model';

const ISSUER = 'https://sso.example.org/realms/home';

function row(over: Partial<OIDCPendingSignIn> = {}): OIDCPendingSignIn {
  return {
    id: 4,
    issuer: ISSUER,
    subject: 'sub-bob',
    email: 'bob@example.org',
    email_verified: false,
    username: 'bob',
    name: 'Bob Smith',
    groups: ['listeners'],
    reason: 'unverified_email',
    first_seen_at: '2026-11-14T10:00:00Z',
    last_seen_at: '2026-11-15T10:00:00Z',
    attempts: 2,
    ...over,
  };
}

const kid: AdminUser = {
  ...sam,
  id: 3,
  username: 'evie',
  managed_by: { id: 1, username: 'chris', display_name: '' },
  member_kind: 'kid',
  member_access: 'all',
};
const demo: AdminUser = { ...sam, id: 4, username: 'demo', is_demo: true };
const bob: AdminUser = { ...sam, id: 5, username: 'bob', email: 'Bob@Example.org' };

describe('waiting sign-ins', () => {
  it('words the known reasons and reads a newer one generically', () => {
    expect(reasonKey('admin_account')).toBe('people.pending.reason.admin_account');
    expect(reasonKey('from_the_future')).toBe('people.pending.reason.unknown');
  });

  it('names a row by its name, then username, then email', () => {
    expect(pendingName(row())).toBe('Bob Smith');
    expect(pendingName(row({ name: ' ' }))).toBe('bob');
    expect(pendingName(row({ name: '', username: '' }))).toBe('bob@example.org');
  });

  it('lists the first groups and counts the rest', () => {
    expect(groupsShown(row({ groups: ['a', 'b', 'c', 'd', 'e'] }))).toEqual({
      shown: ['a', 'b', 'c'],
      more: 2,
    });
    expect(groupsShown(row({ groups: null }))).toEqual({ shown: [], more: 0 });
  });

  it('offers no member, demo or already linked account, but does offer an admin', () => {
    const all = [admin, sam, kid, demo, bob];
    expect(linkCandidates(all, row(), { [ISSUER]: [2] }).map((u) => u.id)).toEqual([1, 5]);
    // Linked to another provider only: still offered.
    expect(linkCandidates(all, row(), { 'https://other': [2] }).map((u) => u.id)).toEqual([
      1, 2, 5,
    ]);
    // A server that sends no linked list.
    expect(linkCandidates(all, row(), undefined).map((u) => u.id)).toEqual([1, 2, 5]);
  });

  it('suggests the one account with the row’s verified email', () => {
    const verified = row({ email_verified: true });
    expect(suggestedAccount([admin, sam, bob], verified)?.id).toBe(5);
    expect(suggestedAccount([admin, bob, { ...bob, id: 6 }], verified)).toBeUndefined();
    expect(suggestedAccount([bob], row({ email: '', email_verified: true }))).toBeUndefined();
  });

  it('suggests no account on an email the provider has not verified', () => {
    expect(suggestedAccount([admin, sam, bob], row())).toBeUndefined();
  });

  it('names an identity by the configured provider, else its issuer’s host', () => {
    const configured = { issuer: ISSUER, name: 'Keycloak' };
    expect(identityProvider({ issuer: ISSUER }, configured)).toBe('Keycloak');
    expect(identityProvider({ issuer: 'https://old.example.net/x' }, configured)).toBe(
      'old.example.net',
    );
    expect(identityProvider({ issuer: 'not a url' }, undefined)).toBe('not a url');
  });

  it('names the provider a person signs in with only while it is the one the server runs', () => {
    const on = { configured: true, issuer: ISSUER, name: 'Keycloak' };
    const mine = [{ issuer: 'https://old.example.net/x' }, { issuer: ISSUER }];
    expect(signInProvider(mine, on)).toBe('Keycloak');
    // Not enabled or not able to run: no identity counts, as on the server.
    expect(signInProvider(mine, { ...on, configured: false })).toBeUndefined();
    // Only another provider's identity, none at all, or the status not read yet.
    expect(signInProvider([{ issuer: 'https://old.example.net/x' }], on)).toBeUndefined();
    expect(signInProvider([], on)).toBeUndefined();
    expect(signInProvider(mine, undefined)).toBeUndefined();
  });
});
