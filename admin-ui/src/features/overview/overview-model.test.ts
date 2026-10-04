import type { ListeningRow, ListeningSession } from '@/api/types';
import { RECENT_LIMIT, greetingFor, splitListening } from './overview-model';

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

function session(over: Partial<ListeningSession>): ListeningSession {
  return {
    ...row({}),
    id: 1,
    device_id: 1,
    device_name: 'iPhone',
    client: null,
    started_at: new Date(now - 600_000).toISOString(),
    last_at: new Date(now).toISOString(),
    start_position: 0,
    speed: 1,
    listened: 600,
    codec: 'aac',
    transcoded: false,
    state: 'playing',
    ...over,
  } as ListeningSession;
}

describe('splitListening', () => {
  it('takes who is live from the sessions and leaves their books out of recent', () => {
    const live = row({ path: 'live' });
    const older = row({ path: 'older', updated_at: new Date(now - 3_600_000).toISOString() });
    const done = row({ path: 'done', finished: true });
    const s = splitListening([older, done, live], [session({ path: 'live' })]);
    expect(s.live.map((x) => x.path)).toEqual(['live']);
    expect(s.recent.map((r) => r.path)).toEqual(['done', 'older']);
    expect(s.inProgress).toBe(2);
  });

  it('counts people, not devices, as listeners, playing first', () => {
    const s = splitListening(
      [],
      [
        session({ id: 1, user_id: 1, state: 'paused' }),
        session({ id: 2, user_id: 1, path: 'b' }),
        session({ id: 3, user_id: 2, path: 'c' }),
      ],
    );
    expect(s.live.map((x) => x.id)).toEqual([2, 3, 1]);
    expect(s.listeners).toBe(2);
  });

  it('caps the recent list', () => {
    const rows = Array.from({ length: RECENT_LIMIT + 5 }, (_, i) => row({ path: `p${i}` }));
    expect(splitListening(rows, []).recent).toHaveLength(RECENT_LIMIT);
  });

  it('handles an empty feed', () => {
    expect(splitListening([], [])).toEqual({ live: [], listeners: 0, recent: [], inProgress: 0 });
  });
});

describe('greetingFor', () => {
  it('follows the hour', () => {
    expect(greetingFor(8)).toBe('morning');
    expect(greetingFor(12)).toBe('afternoon');
    expect(greetingFor(18)).toBe('evening');
  });
});
