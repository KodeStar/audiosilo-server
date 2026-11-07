import type { MatchChange, MatchRun, MatchRunItem, MatchRunMode, MatchScope } from '@/api/types';
import { rangeOf } from '@/features/library/books/use-selection';

// Bulk matching on Health > Not matched (STYLEGUIDE.md "Health triage"): what the
// card shows for the newest run, which books an apply takes, and how a
// marketplace is named. Kept out of the components so the rules are testable.

/** What the card shows: a way to start, a run working, or one waiting for review. */
export type BulkPhase = 'idle' | 'matching' | 'ready' | 'applying';

export function bulkPhase(run: MatchRun | undefined): BulkPhase {
  switch (run?.status) {
    case 'matching':
    case 'applying':
      return run.status;
    // A ready run with nothing left to apply is as good as done.
    case 'ready':
      return run.counts.pending + run.counts.review > 0 ? 'ready' : 'idle';
    default:
      return 'idle';
  }
}

/** What the run is counting: the books matched, or (applying) the matches applied. */
export function runProgress(run: MatchRun): { done: number; total: number } {
  return run.status === 'applying'
    ? { done: run.apply_done, total: run.apply_total }
    : { done: run.done, total: run.total };
}

/** How far the run has got, 0..1 (undefined while it has nothing to count). */
export function runFraction(run: MatchRun): number | undefined {
  const { done, total } = runProgress(run);
  return total > 0 ? done / total : undefined;
}

/**
 * The admin's picks in the review: confident books left out, and books that
 * needed review put in.
 */
export interface Picks {
  exclude: ReadonlySet<number>;
  include: ReadonlySet<number>;
}

export const NO_PICKS: Picks = { exclude: new Set(), include: new Set() };

export const withId = (set: ReadonlySet<number>, id: number) => new Set(set).add(id);
export const withoutId = (set: ReadonlySet<number>, id: number) => {
  const next = new Set(set);
  next.delete(id);
  return next;
};

/** Whether an apply takes the item: confident unless left out, a review one only if put in. */
export function isChosen(
  item: MatchRunItem,
  picks: Picks,
  mode: MatchRunMode = 'match',
  scope: MatchScope = 'fill',
): boolean {
  if (!isPickable(item, mode, scope)) return false;
  return item.outcome === 'auto' ? !picks.exclude.has(item.id) : picks.include.has(item.id);
}

/**
 * Whether the item has a checkbox at all: a candidate, not applied yet, its book
 * still there and not matched since the run (an apply leaves that one alone).
 */
export function isPickable(
  item: MatchRunItem,
  mode: MatchRunMode = 'match',
  scope: MatchScope = 'fill',
): boolean {
  const change = item.changes[scope];
  return (
    (item.outcome === 'auto' || item.outcome === 'review') &&
    !item.applied &&
    !item.gone &&
    !item.matched_since &&
    // A refresh may heal its recorded match without changing fields or art.
    // Match and repick may still attach or change an identifier.
    (mode !== 'refresh' ||
      Boolean(change && (change.match || change.cover || change.fields.length)))
  );
}

/** Flips one item's pick. */
export function togglePick(picks: Picks, item: MatchRunItem): Picks {
  const flip = (set: ReadonlySet<number>) =>
    set.has(item.id) ? withoutId(set, item.id) : withId(set, item.id);
  return item.outcome === 'auto'
    ? { ...picks, exclude: flip(picks.exclude) }
    : { ...picks, include: flip(picks.include) };
}

/**
 * A Shift-click on `item`: every pickable item from the last one clicked
 * (`anchor`, an item id) to this one, in `shown` (the tab's list as shown),
 * takes the state this one takes; the ones in between with no checkbox, or
 * matched by hand from the review (`byHand`, ids: leaveOut took them out and
 * their rows may still show a checkbox until the list reads again), are
 * skipped. Just this one flips when the anchor isn't in the list (none yet,
 * or clicked on another tab).
 */
export function togglePickRange(
  picks: Picks,
  shown: readonly MatchRunItem[],
  anchor: number | undefined,
  item: MatchRunItem,
  byHand: ReadonlySet<number> = new Set(),
  mode: MatchRunMode = 'match',
  scope: MatchScope = 'fill',
): Picks {
  const chosen = !isChosen(item, picks, mode, scope);
  const exclude = new Set(picks.exclude);
  const include = new Set(picks.include);
  for (const it of rangeOf(shown, anchor, item, (x) => x.id)) {
    if (!isPickable(it, mode, scope) || (it !== item && byHand.has(it.id))) continue;
    // A confident one is in unless left out; a review one is out unless put in.
    const [ids, on] = it.outcome === 'auto' ? [exclude, !chosen] : [include, chosen];
    if (on) ids.add(it.id);
    else ids.delete(it.id);
  }
  return { ...picks, exclude, include };
}

/**
 * Leaves an item out of the apply, as a book matched by hand from the review is
 * (the server leaves it alone anyway: this keeps the apply's count true). One
 * already applied is no pick at all (applyCount counts only pending ones).
 */
export function leaveOut(picks: Picks, item: MatchRunItem): Picks {
  if (item.applied) return picks;
  return item.outcome === 'auto'
    ? { ...picks, exclude: withId(picks.exclude, item.id) }
    : { ...picks, include: withoutId(picks.include, item.id) };
}

/**
 * The picks an apply sends: every loaded item without a checkbox under this
 * mode and scope (gone, matched since the run, a refresh with nothing to
 * write) left out, so the apply's count doesn't promise them. Derived, never
 * stored, so changing scope restores the admin's own picks.
 */
export function picksForScope(
  picks: Picks,
  items: readonly MatchRunItem[],
  mode: MatchRunMode,
  scope: MatchScope,
): Picks {
  const exclude = new Set(picks.exclude);
  const include = new Set(picks.include);
  for (const item of items) {
    if (item.applied || isPickable(item, mode, scope)) continue;
    if (item.outcome === 'auto') exclude.add(item.id);
    include.delete(item.id);
  }
  return { exclude, include };
}

/** Keep this margin aligned with internal/matchrun.AutoLead (Classify). */
const AUTO_LEAD = 10;

export function hasCloseScore(item: MatchRunItem): boolean {
  return (
    item.outcome === 'review' &&
    item.runner_up > 0 &&
    item.score - item.runner_up < AUTO_LEAD &&
    item.detail !== 'length_differs' &&
    item.detail !== 'dramatization_only'
  );
}

/** How many books an apply writes to: the confident ones still pending, less left out, plus put in. */
export function applyCount(run: MatchRun, picks: Picks): number {
  return Math.max(0, run.counts.pending - picks.exclude.size) + picks.include.size;
}

/** The apply request for the picks. */
export function applyRequest(scope: MatchScope, picks: Picks) {
  return { scope, exclude: [...picks.exclude], include: [...picks.include] };
}

/** One thing a review row says a scope changes: the cover, a field, or the community match. */
export type ChangePart = { kind: 'cover' } | { kind: 'field'; field: string } | { kind: 'match' };

/**
 * What a review row lists a change as: the cover, then the fields, then the
 * community match when it is what matches the book (a store listing with no ASIN
 * or ISBN, or a refresh healing a merged work). Beside an ASIN or ISBN it is
 * implied, so it isn't listed.
 */
export function changeParts(change: MatchChange | undefined): ChangePart[] {
  if (!change) return [];
  const parts: ChangePart[] = [
    ...(change.cover ? [{ kind: 'cover' } as const] : []),
    ...change.fields.map((field) => ({ kind: 'field', field }) as const),
  ];
  if (change.match && !change.fields.some((f) => f === 'asin' || f === 'isbn')) {
    parts.push({ kind: 'match' });
  }
  return parts;
}
