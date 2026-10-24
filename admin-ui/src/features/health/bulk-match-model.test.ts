import type { MatchRun, MatchRunItem } from '@/api/types';
import { regionName, regionOptions, regionTag } from '@/lib/regions';
import {
  NO_PICKS,
  applyCount,
  leaveOutStale,
  applyRequest,
  bulkPhase,
  isChosen,
  isPickable,
  leaveOut,
  runFraction,
  togglePick,
  togglePickRange,
} from './bulk-match-model';

function run(over: Partial<MatchRun> = {}, counts: Partial<MatchRun['counts']> = {}): MatchRun {
  return {
    id: 1,
    library_id: null,
    mode: 'match',
    region: 'uk',
    status: 'ready',
    started_by: 1,
    started_at: '2026-10-07T10:00:00Z',
    finished_at: '2026-10-07T10:05:00Z',
    total: 10,
    done: 10,
    scope: '',
    apply_total: 0,
    apply_done: 0,
    applied_at: null,
    ...over,
    counts: {
      auto: 4,
      pending: 4,
      review: 2,
      none: 3,
      error: 1,
      applied: 0,
      skipped: 0,
      failed: 0,
      ...counts,
    },
  };
}

function item(over: Partial<MatchRunItem> = {}): MatchRunItem {
  return {
    id: 7,
    run_id: 1,
    library_id: 1,
    path: 'A/B',
    outcome: 'auto',
    score: 95,
    runner_up: 0,
    proposal: { values: {} },
    applied: '',
    book: { title: 'B', author: 'A' },
    changes: {},
    ...over,
  };
}

describe('bulkPhase', () => {
  it('follows the newest run', () => {
    expect(bulkPhase(undefined)).toBe('idle');
    expect(bulkPhase(run({ status: 'matching' }))).toBe('matching');
    expect(bulkPhase(run({ status: 'applying' }))).toBe('applying');
    expect(bulkPhase(run())).toBe('ready');
    for (const status of ['applied', 'cancelled', 'failed', 'interrupted'] as const) {
      expect(bulkPhase(run({ status }))).toBe('idle');
    }
  });

  it('treats a ready run with nothing left to apply as done', () => {
    expect(bulkPhase(run({}, { pending: 0, review: 0 }))).toBe('idle');
    expect(bulkPhase(run({}, { pending: 0, review: 1 }))).toBe('ready');
  });
});

it('runFraction counts matching, then applying', () => {
  expect(runFraction(run({ status: 'matching', done: 3, total: 12 }))).toBe(0.25);
  expect(runFraction(run({ status: 'applying', apply_done: 1, apply_total: 4 }))).toBe(0.25);
  expect(runFraction(run({ status: 'matching', total: 0, done: 0 }))).toBeUndefined();
});

describe('picks', () => {
  const confident = item({ id: 1, outcome: 'auto' });
  const doubtful = item({ id: 2, outcome: 'review' });
  const none = item({ id: 3, outcome: 'none' });

  it('ticks confident matches and leaves the rest to the admin', () => {
    expect(isChosen(confident, NO_PICKS)).toBe(true);
    expect(isChosen(doubtful, NO_PICKS)).toBe(false);
    expect(isChosen(none, NO_PICKS)).toBe(false);
    let picks = togglePick(NO_PICKS, confident);
    picks = togglePick(picks, doubtful);
    expect(isChosen(confident, picks)).toBe(false);
    expect(isChosen(doubtful, picks)).toBe(true);
    expect(applyRequest('fill', picks)).toEqual({ scope: 'fill', exclude: [1], include: [2] });
    // Toggling twice undoes it.
    expect(isChosen(confident, togglePick(picks, confident))).toBe(true);
  });

  it('never offers an applied, gone or candidate-less book', () => {
    expect(isPickable(confident)).toBe(true);
    expect(isPickable(doubtful)).toBe(true);
    expect(isPickable(none)).toBe(false);
    expect(isPickable(item({ applied: 'applied' }))).toBe(false);
    expect(isPickable(item({ gone: true }))).toBe(false);
    expect(isChosen(item({ applied: 'applied' }), NO_PICKS)).toBe(false);
  });

  it("doesn't count a loaded confident book the apply would leave alone", () => {
    const r = run({}, { auto: 4, pending: 3 });
    const loaded = [
      item({ id: 11, outcome: 'auto' }),
      item({ id: 12, outcome: 'auto', matched_since: true }),
      item({ id: 13, outcome: 'auto', gone: true }),
      item({ id: 14, outcome: 'auto', matched_since: true, applied: 'applied' }),
      item({ id: 15, outcome: 'review', matched_since: true }),
    ];
    const picks = leaveOutStale(NO_PICKS, loaded);
    expect([...picks.exclude].sort()).toEqual([12, 13]);
    expect(applyCount(r, picks)).toBe(1);
    // Nothing new: the same picks, so a re-render changes nothing.
    expect(leaveOutStale(picks, loaded)).toBe(picks);
  });

  it('counts what an apply takes: pending confident, less left out, plus put in', () => {
    const r = run({}, { auto: 4, pending: 3 });
    expect(applyCount(r, NO_PICKS)).toBe(3);
    expect(applyCount(r, { exclude: new Set([1, 2]), include: new Set([9]) })).toBe(2);
    expect(applyCount(r, { exclude: new Set([1, 2, 3, 4]), include: new Set() })).toBe(0);
  });

  it('leaves a book matched by hand out of the apply, and never offers one matched since', () => {
    const picks = leaveOut(leaveOut(togglePick(NO_PICKS, doubtful), doubtful), confident);
    expect(isChosen(confident, picks)).toBe(false);
    expect(isChosen(doubtful, picks)).toBe(false);
    expect(applyRequest('fill', picks)).toEqual({ scope: 'fill', exclude: [1], include: [] });
    expect(isPickable(item({ matched_since: true }))).toBe(false);
    // One already applied isn't pending, so leaving it out doesn't lower the count.
    expect(leaveOut(NO_PICKS, item({ applied: 'applied' }))).toBe(NO_PICKS);
  });
});

describe('Shift-click ranges', () => {
  // A tab's list as shown: confident ones, one applied and one gone between them.
  const shown = [
    item({ id: 1 }),
    item({ id: 2 }),
    item({ id: 3, applied: 'applied' }),
    item({ id: 4, gone: true }),
    item({ id: 5 }),
    item({ id: 6 }),
  ];
  const chosen = (picks: Parameters<typeof isChosen>[1]) =>
    shown.filter((it) => isChosen(it, picks)).map((it) => it.id);

  it('gives every pickable item from the last clicked to this one the state this one takes', () => {
    // Clicked 1 (left out), then Shift on 5: 2 and 5 are left out too, 3 and 4 skipped.
    const one = togglePick(NO_PICKS, shown[0]);
    const range = togglePickRange(one, shown, 1, shown[4]);
    expect(chosen(range)).toEqual([6]);
    expect([...range.exclude].sort()).toEqual([1, 2, 5]);
    // Clicked 6 (left out), then Shift back on 2 (out): 2 to 6 go back in.
    const back = togglePickRange(togglePick(range, shown[5]), shown, 6, shown[1]);
    expect(chosen(back)).toEqual([2, 5, 6]);
    expect([...back.exclude]).toEqual([1]);
  });

  it('puts review items in, as their own toggle does', () => {
    const review = shown.map((it) => ({ ...it, outcome: 'review' as const }));
    const range = togglePickRange(togglePick(NO_PICKS, review[5]), review, 6, review[0]);
    expect([...range.include].sort()).toEqual([1, 2, 5, 6]);
    expect(range.exclude.size).toBe(0);
  });

  it('flips just the one item without an anchor in the list', () => {
    expect(chosen(togglePickRange(NO_PICKS, shown, undefined, shown[4]))).toEqual([1, 2, 6]);
    // The anchor clicked on another tab isn't in this one.
    expect(chosen(togglePickRange(NO_PICKS, shown, 99, shown[4]))).toEqual([1, 2, 6]);
  });

  it('leaves an item already in the state alone', () => {
    const picks = togglePickRange(NO_PICKS, shown, 1, shown[1]);
    // Item 2 flips out, so 1 goes out with it; nothing is put in.
    expect([...picks.exclude].sort()).toEqual([1, 2]);
    expect(picks.include.size).toBe(0);
  });

  it('passes over a book matched by hand, which the apply leaves alone', () => {
    // Confident 2 matched by hand (left out); 1 and 6 out, then Shift on 6 from 1: back in.
    const out = togglePick(togglePick(leaveOut(NO_PICKS, shown[1]), shown[0]), shown[5]);
    const range = togglePickRange(out, shown, 1, shown[5], new Set([2]));
    expect(chosen(range)).toEqual([1, 5, 6]);
    expect([...range.exclude]).toEqual([2]);
  });
});

describe('regions', () => {
  it('names a marketplace by its country, the UK included', () => {
    expect(regionName('uk', 'en')).toBe('United Kingdom');
    expect(regionName('de', 'de')).toBe('Deutschland');
    expect(regionTag('uk')).toBe('UK');
    expect(regionTag(undefined)).toBe('');
  });

  it('lists every marketplace with its store', () => {
    const options = regionOptions('en');
    expect(options).toHaveLength(11);
    expect(options[1]).toEqual({ value: 'uk', label: 'United Kingdom (audible.co.uk)' });
  });
});
