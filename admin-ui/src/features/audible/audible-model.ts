import type { TFunction } from 'i18next';
import type { AudibleAccount, AudibleTitle, AudibleTitleState } from '@/api/types';
import type { StatusTone } from '@/components/status-text';
import { isOpen } from '@/features/bridge/bridge-model';

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

/** A title's name: its title, else its ASIN. */
export function titleName(x: Pick<AudibleTitle, 'title' | 'asin'>): string {
  return x.title || x.asin;
}

/** A title's authors and series in one line ("Brandon Sanderson · Mistborn 1"). */
export function titleByline(title: Pick<AudibleTitle, 'authors' | 'series'>): string {
  const parts = [title.authors.join(', ')];
  const s = title.series[0];
  if (s) parts.push(s.position ? `${s.name} ${s.position}` : s.name);
  return parts.filter(Boolean).join(' · ');
}

// Backups (Bridge Phase 4b): "Back up", "Not this book", a destination, the
// account's policy and template, "Back up everything".

/** The codes a failed or parked backup may name, each worded as `audible.code.<code>`. */
export const IMPORT_CODES = [
  'not_released',
  'no_rights',
  'audible_refused',
  'not_writable',
  'exists',
  'disk_full',
  'offline_url_expired',
  'unknown_account',
] as const;

/** Whether a title's backup is waiting or running (a pending request included). */
export function importOpen(x: Pick<AudibleTitle, 'import'>): boolean {
  const s = x.import?.state;
  return s === 'pending' || (s !== undefined && isOpen(s));
}

/**
 * Whether "Back up" is offered for a title: one the account can download (or a
 * preorder, which waits for its release), not on this server (or marked "Not
 * this book", whose match isn't it), with no backup waiting or running.
 */
export function canBackUp(x: AudibleTitle): boolean {
  return (
    (x.state === 'available' || x.state === 'preorder') &&
    (!x.match || x.rejected) &&
    !importOpen(x)
  );
}

/** How a title's backup reads: its i18n key, the status dot, and its code's words. */
export function importStatus(
  x: AudibleTitle,
): { key: string; tone: StatusTone; code?: string } | null {
  if (x.needs_attention) return { key: 'audible.import.attention', tone: 'warn' };
  const imp = x.import;
  if (!imp) return null;
  const code = (IMPORT_CODES as readonly string[]).includes(imp.code)
    ? `audible.code.${imp.code}`
    : undefined;
  switch (imp.state) {
    case 'pending':
      return { key: 'audible.import.pending', tone: 'off' };
    case 'queued':
    case 'claimed':
      return { key: 'audible.import.queued', tone: 'off' };
    case 'running':
      return { key: 'audible.import.running', tone: 'ok' };
    case 'needs_attention':
      return { key: 'audible.import.parked', tone: 'warn', code };
    case 'done':
      return { key: 'audible.import.done', tone: 'ok' };
    case 'failed':
      return { key: 'audible.import.failed', tone: 'bad', code };
    case 'cancelled':
      return { key: 'audible.import.cancelled', tone: 'off' };
  }
}

/** A destination as typed: trimmed, without enclosing slashes ("" = the account's placement). */
export function cleanDest(dest: string): string {
  return dest.trim().replace(/^\/+|\/+$/g, '');
}
