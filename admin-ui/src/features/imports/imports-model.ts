import type {
  AbsUser,
  Import,
  ImportCutoff,
  ImportMapping,
  ImportSummary,
  UnmatchedItem,
} from '@/api/types';
import { formatDateTime, formatDay, hostOf } from '@/lib/format';

// Settings > Import: the rules the steps share. Which AudioSilo user each
// Audiobookshelf account goes to, which sessions a cutoff keeps, how a summary
// and a failure read, and which imports still wait for a decision.

/** Audiobookshelf account id -> the AudioSilo user it goes to (null = not imported). */
export type UserMap = Record<string, number | null>;

/**
 * The mapping a connect starts with: each account to the AudioSilo user of the
 * same name. A user two accounts suggest goes to the first only (the server
 * refuses a user mapped twice).
 */
export function initialMap(users: AbsUser[], known: ReadonlySet<number>): UserMap {
  const taken = new Set<number>();
  const map: UserMap = {};
  for (const u of users) {
    const id = u.suggested_user_id;
    if (id != null && known.has(id) && !taken.has(id)) {
      taken.add(id);
      map[u.abs_id] = id;
    } else map[u.abs_id] = null;
  }
  return map;
}

/** The AudioSilo users more than one account is mapped to. */
export function duplicateUsers(map: UserMap): Set<number> {
  const seen = new Set<number>();
  const dup = new Set<number>();
  for (const id of Object.values(map)) {
    if (id == null) continue;
    if (seen.has(id)) dup.add(id);
    seen.add(id);
  }
  return dup;
}

/** Why a mapping can't start (an i18n key), or undefined when it can. */
export function mappingProblem(map: UserMap): string | undefined {
  if (!Object.values(map).some((id) => id != null)) return 'imports.map.noneMapped';
  if (duplicateUsers(map).size) return 'imports.map.duplicate';
  return undefined;
}

/**
 * The mapped pairs, as POST /admin/imports/abs takes them, each with the
 * account's username (it names the import until the fetch reads its own).
 */
export function mappingBody(map: UserMap, users: AbsUser[]): ImportMapping[] {
  const names = new Map(users.map((u) => [u.abs_id, u.username]));
  return Object.entries(map).flatMap(([abs, id]) =>
    id == null ? [] : [{ abs_user_id: abs, abs_username: names.get(abs) ?? '', user_id: id }],
  );
}

/** How a cutoff is chosen (when starting, and when changing it in review). */
export type CutoffChoice = 'auto' | 'all' | 'date';

/**
 * The cutoff a choice sends: "auto", null (everything) or the day itself
 * (YYYY-MM-DD: the server reads it as the start of that day in its own time);
 * undefined when a day is chosen but none is entered.
 */
export function cutoffValue(choice: CutoffChoice, day: string): ImportCutoff | undefined {
  if (choice === 'auto') return 'auto';
  if (choice === 'all') return null;
  return /^\d{4}-\d{2}-\d{2}$/.test(day) ? day : undefined;
}

/** A stored cutoff and the server's offset from UTC there (minutes), as an import carries them. */
type StoredCutoff = Pick<Import, 'cutoff' | 'cutoff_utc_offset'>;

/**
 * The server's wall clock at a stored cutoff, as an ISO string to read in UTC:
 * the moment moved by the server's offset there. A chosen day is its start in
 * the server's time, which the browser's zone would put on another day (or
 * another hour) when the two differ. Without an offset, the browser's own.
 */
function serverClock(iso: string, offset: number | null): string | undefined {
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return undefined;
  const minutes = offset ?? -new Date(t).getTimezoneOffset();
  return new Date(t + minutes * 60_000).toISOString();
}

/** A stored cutoff as the date input shows it: the server's day (YYYY-MM-DD); "" for none. */
export function cutoffDay(c: StoredCutoff): string {
  return (c.cutoff && serverClock(c.cutoff, c.cutoff_utc_offset)?.slice(0, 10)) ?? '';
}

/** Whether a stored cutoff falls at the server's midnight (a chosen day, not a moment). */
function atServerMidnight(c: StoredCutoff): boolean {
  const clock = c.cutoff ? serverClock(c.cutoff, c.cutoff_utc_offset) : undefined;
  return clock?.slice(11, 19) === '00:00:00';
}

/**
 * Whether a review's cutoff edit changes anything: "auto" always might (it is
 * worked out again), a day does when the date input showed another one, or when
 * the stored cutoff is a moment within that day (an "auto" one), which the day
 * moves to the day's start.
 */
export function cutoffChanged(current: StoredCutoff, next: ImportCutoff): boolean {
  if (next === 'auto') return true;
  if (current.cutoff === null || next === null) return current.cutoff !== next;
  return cutoffDay(current) !== next || !atServerMidnight(current);
}

/**
 * A stored cutoff as the review words it, in the server's time: the date alone
 * when it falls at the server's midnight (a chosen day), else the date and time
 * (an "auto" cutoff is the moment of the first listening here, so earlier
 * sessions that same day count). "" for none.
 */
export function cutoffText(c: StoredCutoff, lang: string): string {
  const clock = c.cutoff ? serverClock(c.cutoff, c.cutoff_utc_offset) : undefined;
  if (!clock) return '';
  return atServerMidnight(c)
    ? formatDay(clock, lang, { day: 'numeric', month: 'short', year: 'numeric' })
    : formatDateTime(clock, lang, true, 'UTC');
}

/** Books matched, all tiers together. */
export function matchedTotal(s: ImportSummary): number {
  return s.matched.path + s.matched.asin + s.matched.isbn + s.matched.title;
}

/** The match tiers with any books, in the order the server tries them. */
export function matchTiers(
  s: ImportSummary,
): { tier: keyof ImportSummary['matched']; count: number }[] {
  return (['path', 'asin', 'isbn', 'title'] as const)
    .map((tier) => ({ tier, count: s.matched[tier] }))
    .filter((x) => x.count > 0);
}

/** Whether an import has nothing to add (no session, progress, finish or bookmark). */
export function importEmpty(s: ImportSummary): boolean {
  return !s.sessions && !s.progress && !s.finished && !s.bookmarks && !(s.estimated > 0);
}

/** The i18n key of why a book wasn't matched. */
export function unmatchedReasonKey(reason: UnmatchedItem['reason']): string {
  switch (reason) {
    case 'contested':
    case 'no_access':
    case 'split_discs':
      return `imports.unmatched.reason.${reason}`;
  }
  return 'imports.unmatched.reason.no_match';
}

const FAILURE_CODES = [
  'abs_unreachable',
  'abs_unauthorized',
  'not_abs',
  'interrupted',
  'fetch_failed',
] as const;

/** The i18n key of what to do about a failed import (by its error_code). */
export function failureHintKey(code: string): string {
  return (FAILURE_CODES as readonly string[]).includes(code)
    ? `imports.failure.${code}`
    : 'imports.failure.fetch_failed';
}

/** Imports still waiting for a decision (review cards), and the decided ones (history). */
export function splitImports(list: Import[]): { open: Import[]; done: Import[] } {
  const open: Import[] = [];
  const done: Import[] = [];
  for (const i of list) (i.status === 'applied' || i.status === 'undone' ? done : open).push(i);
  return { open, done };
}

/** The import an apply for this person would replace: their applied one from the same source. */
export function replacedBy(imp: Import, list: Import[]): Import | undefined {
  return list.find(
    (o) =>
      o.id !== imp.id &&
      o.user_id === imp.user_id &&
      o.source === imp.source &&
      o.status === 'applied',
  );
}

/** An address without its scheme, for a line of text ("books.example.com", "nas:13378/abs"). */
export function sourceHost(url: string): string {
  const host = hostOf(url);
  try {
    return host + new URL(url).pathname.replace(/\/+$/, '');
  } catch {
    return host;
  }
}
