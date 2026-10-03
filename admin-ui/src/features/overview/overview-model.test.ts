import type { ListeningRow } from '@/api/types';
import { LIVE_WINDOW_MS, RECENT_LIMIT, greetingFor, splitListening } from './overview-model';

const now = Date.parse('2026-10-03T20:00:00Z');

function row(over: Partial<ListeningRow>): ListeningRow {
  return {
    user_id: 1,
    username: 'sam',
    library_id: 1,
    path: 'a/b',
    title: 'Book',
    author: 'Author',
    position: 10,
    duration: 100,
    finished: false,
    updated_at: new Date(now).toISOString(),
    ...over,
  };
}

describe('splitListening', () => {
  it('treats recent unfinished progress as live', () => {
    const live = row({ path: 'live', updated_at: new Date(now - 60_000).toISOString() });
    const stale = row({
      path: 'stale',
      updated_at: new Date(now - LIVE_WINDOW_MS - 1000).toISOString(),
    });
    const done = row({ path: 'done', finished: true });
    const s = splitListening([stale, done, live], now);
    expect(s.live.map((r) => r.path)).toEqual(['live']);
    expect(s.recent.map((r) => r.path)).toEqual(['done', 'stale']);
    expect(s.inProgress).toBe(2);
  });

  it('counts people, not books, as listeners', () => {
    const rows = [
      row({ user_id: 1, path: 'a' }),
      row({ user_id: 1, path: 'b' }),
      row({ user_id: 2, path: 'c' }),
    ];
    const s = splitListening(rows, now);
    expect(s.live).toHaveLength(3);
    expect(s.listeners).toBe(2);
  });

  it('caps the recent list', () => {
    const rows = Array.from({ length: RECENT_LIMIT + 5 }, (_, i) =>
      row({
        path: `p${i}`,
        updated_at: new Date(now - LIVE_WINDOW_MS - (i + 1) * 1000).toISOString(),
      }),
    );
    expect(splitListening(rows, now).recent).toHaveLength(RECENT_LIMIT);
  });

  it('handles an empty feed', () => {
    expect(splitListening([], now)).toEqual({ live: [], listeners: 0, recent: [], inProgress: 0 });
  });
});

describe('greetingFor', () => {
  it('follows the hour', () => {
    expect(greetingFor(8)).toBe('morning');
    expect(greetingFor(12)).toBe('afternoon');
    expect(greetingFor(18)).toBe('evening');
  });
});
