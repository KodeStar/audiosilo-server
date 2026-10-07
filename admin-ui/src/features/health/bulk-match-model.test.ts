import type { MatchRun, MatchRunItem } from '@/api/types';
import { regionName, regionOptions, regionTag } from '@/lib/regions';
import {
  NO_PICKS,
  applyCount,
  applyRequest,
  bulkPhase,
  isChosen,
  isPickable,
  runFraction,
  togglePick,
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

  it('counts what an apply takes: pending confident, less left out, plus put in', () => {
    const r = run({}, { auto: 4, pending: 3 });
    expect(applyCount(r, NO_PICKS)).toBe(3);
    expect(applyCount(r, { exclude: new Set([1, 2]), include: new Set([9]) })).toBe(2);
    expect(applyCount(r, { exclude: new Set([1, 2, 3, 4]), include: new Set() })).toBe(0);
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
