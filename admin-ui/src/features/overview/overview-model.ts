import type { ListeningRow } from '@/api/types';

/**
 * How recently a progress update must have landed for the row to count as
 * "listening now". Players save progress every few seconds while playing, so a
 * pause longer than this reads as stopped. Phase 4a replaces this with real
 * sessions.
 */
export const LIVE_WINDOW_MS = 10 * 60 * 1000;

/** The recent-listening list on Overview shows at most this many rows. */
export const RECENT_LIMIT = 8;

export interface ListeningSplit {
  live: ListeningRow[];
  /** Distinct people among `live` (one person can have several books in progress). */
  listeners: number;
  recent: ListeningRow[];
  inProgress: number;
}

/**
 * Splits the admin listening feed (newest first from the server, but sorted
 * here too so the UI never depends on it) into who is listening right now and
 * the recent history.
 */
export function splitListening(rows: readonly ListeningRow[], now: number): ListeningSplit {
  const sorted = [...rows].sort((a, b) => Date.parse(b.updated_at) - Date.parse(a.updated_at));
  const isLive = (r: ListeningRow) =>
    !r.finished && now - Date.parse(r.updated_at) <= LIVE_WINDOW_MS;
  const live = sorted.filter(isLive);
  return {
    live,
    listeners: new Set(live.map((r) => r.user_id)).size,
    recent: sorted.filter((r) => !isLive(r)).slice(0, RECENT_LIMIT),
    inProgress: sorted.filter((r) => !r.finished).length,
  };
}

export type Greeting = 'morning' | 'afternoon' | 'evening';

export function greetingFor(hour: number): Greeting {
  if (hour < 12) return 'morning';
  if (hour < 18) return 'afternoon';
  return 'evening';
}
