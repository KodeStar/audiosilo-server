import type { MatchRun, MatchRunItem, MatchScope } from '@/api/types';

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

/** Whether an apply takes the item: confident unless left out, a review one only if put in. */
export function isChosen(item: MatchRunItem, picks: Picks): boolean {
  if (!isPickable(item)) return false;
  return item.outcome === 'auto' ? !picks.exclude.has(item.id) : picks.include.has(item.id);
}

/** Whether the item has a checkbox at all: a candidate, not applied yet, its book still there. */
export function isPickable(item: MatchRunItem): boolean {
  return (item.outcome === 'auto' || item.outcome === 'review') && !item.applied && !item.gone;
}

/** Flips one item's pick. */
export function togglePick(picks: Picks, item: MatchRunItem): Picks {
  const flip = (set: ReadonlySet<number>) => {
    const next = new Set(set);
    if (next.has(item.id)) next.delete(item.id);
    else next.add(item.id);
    return next;
  };
  return item.outcome === 'auto'
    ? { ...picks, exclude: flip(picks.exclude) }
    : { ...picks, include: flip(picks.include) };
}

/** How many books an apply writes to: the confident ones still pending, less left out, plus put in. */
export function applyCount(run: MatchRun, picks: Picks): number {
  return Math.max(0, run.counts.pending - picks.exclude.size) + picks.include.size;
}

/** The apply request for the picks. */
export function applyRequest(scope: MatchScope, picks: Picks) {
  return { scope, exclude: [...picks.exclude], include: [...picks.include] };
}
