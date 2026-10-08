import { describe, expect, it } from 'vitest';
import { importBusy } from '@/api/hooks';
import type { AbsUser, Import, ImportSummary } from '@/api/types';
import {
  cutoffChanged,
  cutoffDay,
  cutoffText,
  cutoffValue,
  duplicateUsers,
  failureHintKey,
  importEmpty,
  initialMap,
  mappingBody,
  mappingProblem,
  matchTiers,
  matchedTotal,
  replacedBy,
  sourceHost,
  splitImports,
  unmatchedReasonKey,
} from './imports-model';

const absUser = (abs_id: string, suggested: number | null): AbsUser => ({
  abs_id,
  username: abs_id,
  type: 'user',
  suggested_user_id: suggested,
});

function summary(over: Partial<ImportSummary> = {}): ImportSummary {
  return {
    items: 10,
    matched: { path: 6, asin: 1, isbn: 0, title: 2 },
    unmatched: 1,
    sessions: 40,
    skipped_after_cutoff: 0,
    listened: 36_000,
    estimated: 0,
    progress: 9,
    finished: 3,
    bookmarks: 2,
    first_listen: '2024-01-01T10:00:00Z',
    last_listen: '2026-01-01T10:00:00Z',
    ...over,
  };
}

function imp(over: Partial<Import> = {}): Import {
  return {
    id: 1,
    user_id: 2,
    username: 'sam',
    source: 'abs',
    source_url: 'https://abs.example.com',
    source_user: 'sam',
    status: 'review',
    cutoff: null,
    cutoff_utc_offset: null,
    created_at: '2026-10-08T10:00:00Z',
    applied_at: null,
    error: '',
    error_code: '',
    summary: summary(),
    ...over,
  };
}

describe('user mapping', () => {
  it('starts from the suggestions, each AudioSilo user once and only known users', () => {
    const map = initialMap(
      [absUser('a', 2), absUser('b', 2), absUser('c', null), absUser('d', 99)],
      new Set([1, 2]),
    );
    expect(map).toEqual({ a: 2, b: null, c: null, d: null });
  });

  it('needs at least one person and no person twice', () => {
    expect(mappingProblem({ a: null, b: null })).toBe('imports.map.noneMapped');
    expect(mappingProblem({})).toBe('imports.map.noneMapped');
    expect(mappingProblem({ a: 2, b: 2 })).toBe('imports.map.duplicate');
    expect(duplicateUsers({ a: 2, b: 2, c: 3, d: null })).toEqual(new Set([2]));
    expect(mappingProblem({ a: 2, b: 3, c: null })).toBeUndefined();
  });

  it('sends only the mapped pairs, each with its account name', () => {
    const accounts = [
      { abs_id: 'a', username: 'ann', type: 'user', suggested_user_id: null },
      { abs_id: 'b', username: 'bob', type: 'user', suggested_user_id: null },
    ];
    expect(mappingBody({ a: 2, b: null, c: 3 }, accounts)).toEqual([
      { abs_user_id: 'a', abs_username: 'ann', user_id: 2 },
      { abs_user_id: 'c', abs_username: '', user_id: 3 },
    ]);
  });
});

describe('cutoff', () => {
  it('maps a choice to what the server takes: "auto", null or the day itself', () => {
    expect(cutoffValue('auto', '2025-01-01')).toBe('auto');
    expect(cutoffValue('all', '2025-01-01')).toBeNull();
    expect(cutoffValue('date', '2025-03-04')).toBe('2025-03-04');
    expect(cutoffValue('date', '')).toBeUndefined();
    expect(cutoffValue('date', 'nope')).toBeUndefined();
  });

  // A stored cutoff in a server zone the browser isn't in (whichever it is):
  // UTC+10 and UTC-8 put a server midnight on the browser's previous or next
  // day almost everywhere.
  const east = { cutoff: '2026-01-04T14:00:00Z', cutoff_utc_offset: 600 }; // 5 Jan, 00:00 at UTC+10
  const west = { cutoff: '2026-01-05T08:00:00Z', cutoff_utc_offset: -480 }; // 5 Jan, 00:00 at UTC-8
  const none = { cutoff: null, cutoff_utc_offset: null };

  it('shows a stored cutoff as the server day in the date input', () => {
    expect(cutoffDay(east)).toBe('2026-01-05');
    expect(cutoffDay(west)).toBe('2026-01-05');
    expect(cutoffDay(none)).toBe('');
    // Without the offset, the browser's day.
    const local = new Date(2026, 0, 5, 0, 0, 0).toISOString();
    expect(cutoffDay({ cutoff: local, cutoff_utc_offset: null })).toBe('2026-01-05');
  });

  it('knows when a review edit changes nothing', () => {
    // A day is compared with the stored moment as the date input shows it: the server's day.
    for (const stored of [east, west]) {
      expect(cutoffChanged(stored, '2026-01-05')).toBe(false);
      expect(cutoffChanged(stored, '2026-01-04')).toBe(true);
      expect(cutoffChanged(stored, '2026-01-06')).toBe(true);
      expect(cutoffChanged(stored, null)).toBe(true);
      // "auto" is worked out again by the server, so it always goes.
      expect(cutoffChanged(stored, 'auto')).toBe(true);
    }
    // An "auto" moment (14:37 server time) shows as its day; saving that day moves
    // the cutoff to the day's start, so it is a change.
    const moment = { cutoff: '2025-03-04T04:37:12Z', cutoff_utc_offset: 600 };
    expect(cutoffChanged(moment, '2025-03-04')).toBe(true);
    expect(cutoffChanged(none, null)).toBe(false);
    expect(cutoffChanged(none, '2026-01-05')).toBe(true);
    expect(cutoffChanged(none, 'auto')).toBe(true);
  });

  it('words a chosen day as a date, and an "auto" moment with its time, in server time', () => {
    expect(cutoffText(east, 'en')).toBe('Jan 5, 2026');
    expect(cutoffText(west, 'en')).toBe('Jan 5, 2026');
    // 14:37:12 at UTC+10.
    expect(cutoffText({ cutoff: '2025-03-04T04:37:12Z', cutoff_utc_offset: 600 }, 'en')).toBe(
      'Mar 4, 2025, 2:37 PM',
    );
    // A second past the server's midnight is still a moment, not a day.
    expect(cutoffText({ cutoff: '2026-01-05T08:00:01Z', cutoff_utc_offset: -480 }, 'en')).toBe(
      'Jan 5, 2026, 12:00 AM',
    );
    // The browser's midnight isn't the server's.
    expect(cutoffText({ cutoff: '2026-01-05T00:00:00Z', cutoff_utc_offset: 60 }, 'en')).toBe(
      'Jan 5, 2026, 1:00 AM',
    );
    expect(cutoffText(none, 'en')).toBe('');
  });
});

describe('summary', () => {
  it('totals and lists the match tiers in the order they are tried', () => {
    const s = summary();
    expect(matchedTotal(s)).toBe(9);
    expect(matchTiers(s)).toEqual([
      { tier: 'path', count: 6 },
      { tier: 'asin', count: 1 },
      { tier: 'title', count: 2 },
    ]);
  });

  it('is empty only when it adds nothing at all', () => {
    const none = summary({ sessions: 0, progress: 0, finished: 0, bookmarks: 0, estimated: 0 });
    expect(importEmpty(none)).toBe(true);
    expect(importEmpty({ ...none, bookmarks: 1 })).toBe(false);
    expect(importEmpty({ ...none, estimated: 600 })).toBe(false);
    expect(importEmpty(summary())).toBe(false);
  });

  it('words reasons and failures, with a fallback for unknown codes', () => {
    expect(unmatchedReasonKey('contested')).toBe('imports.unmatched.reason.contested');
    expect(unmatchedReasonKey('no_access')).toBe('imports.unmatched.reason.no_access');
    expect(unmatchedReasonKey('no_match')).toBe('imports.unmatched.reason.no_match');
    expect(unmatchedReasonKey('split_discs')).toBe('imports.unmatched.reason.split_discs');
    expect(failureHintKey('abs_unauthorized')).toBe('imports.failure.abs_unauthorized');
    expect(failureHintKey('interrupted')).toBe('imports.failure.interrupted');
    expect(failureHintKey('something_new')).toBe('imports.failure.fetch_failed');
  });

  it('shows the address without its scheme', () => {
    expect(sourceHost('https://abs.example.com/')).toBe('abs.example.com');
    expect(sourceHost('http://nas:13378/audiobookshelf/')).toBe('nas:13378/audiobookshelf');
    expect(sourceHost('not a url')).toBe('not a url');
  });
});

describe('import lists', () => {
  it('polls only while work is under way', () => {
    expect(importBusy(imp({ status: 'fetching' }))).toBe(true);
    expect(importBusy(imp({ status: 'applying' }))).toBe(true);
    for (const status of ['review', 'applied', 'failed', 'undone'] as const) {
      expect(importBusy(imp({ status }))).toBe(false);
    }
    expect(importBusy(undefined)).toBe(false);
  });

  it('splits open imports from decided ones', () => {
    const list = [
      imp({ id: 1, status: 'fetching' }),
      imp({ id: 2, status: 'applied' }),
      imp({ id: 3, status: 'failed' }),
      imp({ id: 4, status: 'undone' }),
      imp({ id: 5, status: 'review' }),
    ];
    const { open, done } = splitImports(list);
    expect(open.map((i) => i.id)).toEqual([1, 3, 5]);
    expect(done.map((i) => i.id)).toEqual([2, 4]);
  });

  it("finds the person's applied import an apply would replace", () => {
    const next = imp({ id: 5 });
    const old = imp({ id: 2, status: 'applied', applied_at: '2026-01-01T00:00:00Z' });
    expect(replacedBy(next, [next, old])).toBe(old);
    expect(replacedBy(next, [next, { ...old, user_id: 9 }])).toBeUndefined();
    expect(replacedBy(next, [next, { ...old, status: 'undone' }])).toBeUndefined();
  });
});
