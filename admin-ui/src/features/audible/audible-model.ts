import type { TFunction } from 'i18next';
import type { AudibleAccount, AudibleTitle, AudibleTitleState, BridgeJob } from '@/api/types';
import type { StatusTone } from '@/components/status-text';
import { failureCode, num, text } from '@/features/bridge/bridge-model';

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

/**
 * The titles page's "on this server" filter: here, not here, or "Needs a look"
 * (server#372: titles not here whose planned backup warns, preorders first).
 */
export type MatchedFilter = '' | 'matched' | 'missing' | 'look';

/** The titles query's `matched` for a filter ("Needs a look" reads the titles not here). */
export function matchedParam(f: MatchedFilter): 0 | 1 | undefined {
  if (f === 'matched') return 1;
  if (f === 'missing' || f === 'look') return 0;
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
  'drm_free',
] as const;

/**
 * The codes a failed Check now (audible.poll) or place sync (audible.sync) may
 * name, each worded as `audible.pollCode.<code>`: Audible refused the device
 * (sign in again) or the sign-in for now (the next check tries again).
 */
export const POLL_CODES = ['audible_refused', 'audible_sign_in'] as const;

/**
 * The words for an Audible job's failure code in the Bridge's jobs list (an
 * i18n key), undefined for a job of another kind or a code the console
 * doesn't know (its error is shown as the Bridge wrote it). The code is the
 * result's, else (a backup's, as the server's bridge.AudibleImportCode reads
 * it) one its error starts with ("drm_free: ...").
 */
export function audibleJobReason(job: {
  kind: string;
  result?: Record<string, unknown> | null;
  error?: string;
}): string | undefined {
  if (job.kind === 'audible.import') {
    const code = failureCode(job, IMPORT_CODES);
    return code ? `audible.code.${code}` : undefined;
  }
  if (job.kind === 'audible.poll' || job.kind === 'audible.sync') {
    const code = failureCode(job, POLL_CODES);
    return code ? `audible.pollCode.${code}` : undefined;
  }
  return undefined;
}

/**
 * What an ended place sync (audible.sync) did, from its result (the Bridge's
 * audiblesync.Result): nothing because Audible asked the Bridge to wait (until
 * `until`, when it said) or the account's sync was off by then, else how many
 * of the titles had a place on Audible and how many of those moved the place
 * here. A failed sync's counts are what it did before it stopped (`partial`;
 * a Bridge that sends them, bridge#80); undefined for another kind, a job
 * still open, or a result without them.
 */
export type SyncSummary =
  | { kind: 'waiting'; until?: string }
  | { kind: 'off' }
  | { kind: 'counts'; titles: number; heard: number; applied: number; partial: boolean };

export function syncSummary(
  job: Pick<BridgeJob, 'kind' | 'state'> & { result?: Record<string, unknown> | null },
): SyncSummary | undefined {
  const r = job.result;
  if (job.kind !== 'audible.sync' || !r || (job.state !== 'done' && job.state !== 'failed'))
    return undefined;
  if (job.state === 'done' && r.waiting === true)
    return { kind: 'waiting', until: text(r.wait_until) };
  if (job.state === 'done' && r.sync_off === true) return { kind: 'off' };
  const titles = num(r.titles);
  const heard = num(r.heard);
  if (titles === undefined || heard === undefined) return undefined;
  return {
    kind: 'counts',
    titles,
    heard,
    applied: num(r.applied) ?? 0,
    partial: job.state === 'failed',
  };
}

/**
 * Whether a title's backup job is queued or running (not a pending request,
 * not parked): its state, and then its match, change with the job, so the
 * loaded titles are asked again meanwhile.
 */
export function backupRunning(x: Pick<AudibleTitle, 'import'>): boolean {
  const s = x.import?.state;
  return s === 'queued' || s === 'claimed' || s === 'running';
}

/**
 * Whether "Not this book" is offered for a title: not on one whose match is its
 * own backup (a done backup placed the title's own book), always to undo it.
 */
export function canReject(x: Pick<AudibleTitle, 'match' | 'rejected'>): boolean {
  return x.rejected || x.match?.how !== 'import';
}

/** Whether a title is on this server: a match not marked "Not this book". */
export function onServer<T extends Pick<AudibleTitle, 'match' | 'rejected'>>(
  x: T,
): x is T & { match: NonNullable<T['match']> } {
  return x.match !== null && !x.rejected;
}

/**
 * Whether a title's place can sync with Audible (Phase 4c): it is on this
 * server (a match not marked "Not this book"), still the account's and
 * released (not a preorder: the server's syncTitleSQL, bridge#60), not
 * ignored, and not marked "Don't sync this book". The account's own sync
 * switch is separate.
 */
export function canSync(x: AudibleTitle): boolean {
  return onServer(x) && x.state !== 'gone' && x.state !== 'preorder' && !x.ignored && !x.no_sync;
}

/** How a title's backup reads: its i18n key, the status dot, and its code's words. */
export function importStatus(
  x: AudibleTitle,
): { key: string; tone: StatusTone; code?: string } | null {
  if (x.needs_attention) return { key: 'audible.import.attention', tone: 'warn' };
  const imp = x.import;
  if (!imp) return null;
  const code = (IMPORT_CODES as readonly string[]).includes(imp.code)
    ? importCodeKey(imp.code, imp.conflict)
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

/**
 * A backup's code worded: `exists` by what was in the way (a folder that is
 * already a book, or holds audio, is no file at the destination); a conflict
 * this console doesn't know reads as the file.
 */
function importCodeKey(code: string, conflict = ''): string {
  if (code === 'exists' && (conflict === 'book' || conflict === 'audio'))
    return `audible.code.exists.${conflict}`;
  return `audible.code.${code}`;
}

/** A destination as typed: trimmed, without enclosing slashes ("" = the account's placement). */
export function cleanDest(dest: string): string {
  return dest.trim().replace(/^\/+|\/+$/g, '');
}

// What the console says of a title the Bridge left unmatched (4b owner fixes):
// where its backup would go, and why it isn't shown as here.

/** How a title's planned destination reads: its i18n key and values, and whether it warns. */
export interface PlannedLine {
  key: string;
  values: Record<string, string | number>;
  warn: boolean;
  /** The plan used another template than the account has now: it changes at the next check. */
  stale: boolean;
}

const PLAN_CONFLICTS = ['file', 'book', 'audio'];

/**
 * A title's planned destination as one line, null when there is none or the
 * admin set a destination of their own (checked when the backup starts).
 * template is the account's current one (undefined when unknown).
 */
export function plannedLine(
  x: Pick<AudibleTitle, 'planned' | 'dest_path'>,
  template: string | undefined,
): PlannedLine | null {
  const p = x.planned;
  if (!p || x.dest_path) return null;
  const stale = template !== undefined && p.template !== template;
  const line = (key: string, values: Record<string, string | number> = {}, warn = false) => ({
    key,
    values: { path: p.path, ...values },
    warn,
    stale,
  });
  if (p.state === 'error')
    return p.conflict
      ? line('audible.planned.error', { message: p.conflict }, true)
      : line('audible.planned.errorBare', {}, true);
  if (p.state === 'exists')
    return line(
      `audible.planned.exists.${PLAN_CONFLICTS.includes(p.conflict) ? p.conflict : 'file'}`,
      {},
      true,
    );
  if (p.kind === 'series_existing' && p.siblings > 0)
    return line('audible.planned.seriesExisting', { count: p.siblings });
  // A new top folder (with the automatic placement, the author's) or a new
  // series is where the Bridge most likely missed the author or series the
  // library already has under another spelling (server#372): a warning.
  if (p.state === 'new')
    return line(p.template ? 'audible.planned.newFolder' : 'audible.planned.newAuthor', {}, true);
  if (p.kind === 'series_new') return line('audible.planned.seriesNew', {}, true);
  // A state this console doesn't know (a newer Bridge) only names the path.
  if (p.state !== 'joins') return line('audible.planned.standalone');
  // With a template, joins only means the destination's top folder exists;
  // the default placement puts a standalone title in the author's folder.
  return line(p.template ? 'audible.planned.joinsFolder' : 'audible.planned.joinsAuthor');
}

/**
 * Whether a title is one "Needs a look" lists: not on this server, one a backup
 * can still come of (not unavailable or gone from the account), whose planned
 * destination warns (plannedLine).
 */
export function needsLook(x: AudibleTitle, template: string | undefined): boolean {
  if (x.match || x.state === 'unavailable' || x.state === 'gone') return false;
  return plannedLine(x, template)?.warn ?? false;
}

/**
 * "Needs a look"'s titles: those needsLook keeps, preorders first (a preorder
 * backs up on release day without anyone looking), else in the list's order.
 */
export function lookList(
  titles: readonly AudibleTitle[],
  templates: ReadonlyMap<string, string>,
): AudibleTitle[] {
  const look = titles.filter((x) => needsLook(x, templates.get(x.account)));
  return [
    ...look.filter((x) => x.state === 'preorder'),
    ...look.filter((x) => x.state !== 'preorder'),
  ];
}

/** The reason the Bridge sends for a title it left unmatched because another claimed the same book. */
export const REASON_CONTESTED = 'contested';

/**
 * The i18n key a title's reason reads as, null to show it as sent: a machine
 * code the console has no words for reads as a plain sentence, and Audible's
 * own words stay. Contested is null too: the row explains it with its book.
 */
export function reasonKey(reason: string): string | null {
  if (!reason || reason === REASON_CONTESTED) return null;
  return /^[a-z][a-z0-9_]*$/.test(reason) ? 'audible.reason.other' : null;
}

/**
 * The destination field's placeholder: where the Bridge planned the backup at
 * its last check, else an example. Never the field's value: a saved value
 * would freeze a destination of the admin's own.
 */
export function plannedPlaceholder(x: Pick<AudibleTitle, 'planned'>): string {
  return x.planned?.path || 'Author/Series/Title.m4b';
}
