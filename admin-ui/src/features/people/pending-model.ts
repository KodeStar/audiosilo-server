import {
  OIDC_PENDING_REASONS,
  type AdminUser,
  type OIDCIdentity,
  type OIDCPendingList,
  type OIDCPendingSignIn,
} from '@/api/types';
import { hostOf } from '@/lib/format';
import { isIndependentAccount } from './household-model';

// People's waiting sign-ins (server#235): single sign-ons no account took,
// which an admin links to an account, makes one for, or dismisses; and a
// person's linked sign-ons on their page. Pure, so the rules are tested apart
// from the screens.

/** The i18n key for a waiting row's reason; a code from a newer server reads generically. */
export function reasonKey(reason: string): string {
  return (OIDC_PENDING_REASONS as readonly string[]).includes(reason)
    ? `people.pending.reason.${reason}`
    : 'people.pending.reason.unknown';
}

/** What a waiting row is called: the person's name, else their username, else their email. */
export function pendingName(p: OIDCPendingSignIn): string {
  return p.name.trim() || p.username.trim() || p.email || p.subject;
}

/** How many of a row's groups are listed before "and N more". */
const GROUPS_SHOWN = 3;

/** A row's groups as listed: the first few, and how many more there are. */
export function groupsShown(p: OIDCPendingSignIn): { shown: string[]; more: number } {
  const all = p.groups ?? [];
  return { shown: all.slice(0, GROUPS_SHOWN), more: Math.max(0, all.length - GROUPS_SHOWN) };
}

/**
 * The accounts a waiting row can be linked to: not a household member or a
 * demo account (the server refuses those, 409 not_linkable), and not one that
 * already has an identity from the row's provider (409 user_has_identity). An
 * admin's account is offered: an admin may link to one, it is only never
 * linked on its own. In the list's order.
 */
export function linkCandidates(
  users: readonly AdminUser[],
  row: OIDCPendingSignIn,
  linked: OIDCPendingList['linked_user_ids'],
): AdminUser[] {
  const taken = new Set(linked?.[row.issuer] ?? []);
  return users.filter((u) => isIndependentAccount(u) && !taken.has(u.id));
}

/**
 * The account Link suggests: the one candidate whose email is the row's
 * (letter case aside), else none. A suggestion only: the admin still chooses.
 */
export function suggestedAccount(
  candidates: readonly AdminUser[],
  row: OIDCPendingSignIn,
): AdminUser | undefined {
  const email = row.email.trim().toLowerCase();
  if (!email) return undefined;
  const same = candidates.filter((u) => u.email.trim().toLowerCase() === email);
  return same.length === 1 ? same[0] : undefined;
}

/**
 * What a person's linked sign-on is called: the configured provider's name
 * when it is that provider's, else the issuer's host (a provider since
 * replaced), else the issuer as written.
 */
export function identityProvider(
  identity: Pick<OIDCIdentity, 'issuer'>,
  configured: { issuer: string; name: string } | undefined,
): string {
  if (configured && configured.issuer === identity.issuer && configured.name) {
    return configured.name;
  }
  return hostOf(identity.issuer) || identity.issuer;
}
