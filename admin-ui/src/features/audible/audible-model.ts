import type { TFunction } from 'i18next';
import type { AudibleAccount, AudibleTitle, AudibleTitleState } from '@/api/types';
import type { StatusTone } from '@/components/status-text';

// Audible (Bridge Phase 4a): the accounts the Bridge signed in to and the titles
// each owns, read-only in the console (accounts are signed in and mapped on the
// Bridge's own page). The small rules the card and the titles page share.

/** How a title's state reads as a status dot. */
export const TITLE_STATE_TONE: Record<AudibleTitleState, StatusTone> = {
  available: 'ok',
  preorder: 'warn',
  unavailable: 'bad',
  gone: 'off',
};

/** The titles page's "on this server" filter. */
export type MatchedFilter = '' | 'matched' | 'missing';

/** The titles query's `matched` for a filter. */
export function matchedParam(f: MatchedFilter): 0 | 1 | undefined {
  if (f === 'matched') return 1;
  if (f === 'missing') return 0;
  return undefined;
}

/**
 * The titles page's account filter: the account chosen while it is still
 * listed, else none, so one removed on the Bridge (its picker gone with it when
 * one account is left) never leaves the list filtered to nothing.
 */
export function chosenAccount(accounts: readonly AudibleAccount[], chosen: string): string {
  return accounts.some((a) => a.id === chosen) ? chosen : '';
}

/**
 * The account Check now polls on the titles page: the one chosen, else the
 * only one there is; undefined when several and none chosen.
 */
export function pollTarget(
  accounts: readonly AudibleAccount[],
  chosen: string,
): AudibleAccount | undefined {
  if (chosen) return accounts.find((a) => a.id === chosen);
  return accounts.length === 1 ? accounts[0] : undefined;
}

/** An account's name: its label, else its store and person. */
export function accountName(t: TFunction, a: AudibleAccount): string {
  return (
    a.label ||
    t('audible.account.unnamed', { name: a.user.display_name, store: a.marketplace.toUpperCase() })
  );
}

/** A title's authors and series in one line ("Brandon Sanderson · Mistborn 1"). */
export function titleByline(title: Pick<AudibleTitle, 'authors' | 'series'>): string {
  const parts = [title.authors.join(', ')];
  const s = title.series[0];
  if (s) parts.push(s.position ? `${s.name} ${s.position}` : s.name);
  return parts.filter(Boolean).join(' · ');
}
