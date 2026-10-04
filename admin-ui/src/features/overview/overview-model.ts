import type { ListeningRow, ListeningSession } from '@/api/types';
import { liveSummary, sortLive } from '@/features/activity/live-model';
import { refKey } from '@/lib/book-route';

/** The recent-listening list on Overview shows at most this many rows. */
export const RECENT_LIMIT = 8;

export interface ListeningSplit {
  /** The server's live sessions (one per device), playing first. */
  live: ListeningSession[];
  /** Distinct people among `live` (one person can listen on two devices). */
  listeners: number;
  /** The newest progress that isn't live right now. */
  recent: ListeningRow[];
  inProgress: number;
}

const key = (r: { user_id: number; library_id: number; path: string }) =>
  `${r.user_id}\0${refKey(r)}`;

/**
 * Splits the Overview's listening into who is listening right now (the server's
 * live sessions) and the recent history (the admin progress feed, newest first,
 * without the books being listened to now).
 */
export function splitListening(
  rows: readonly ListeningRow[],
  live: readonly ListeningSession[],
): ListeningSplit {
  const sorted = [...rows].sort((a, b) => Date.parse(b.updated_at) - Date.parse(a.updated_at));
  const now = new Set(live.map(key));
  return {
    live: sortLive(live),
    listeners: liveSummary(live).listeners,
    recent: sorted.filter((r) => !now.has(key(r))).slice(0, RECENT_LIMIT),
    inProgress: sorted.filter((r) => !r.finished).length,
  };
}

export type Greeting = 'morning' | 'afternoon' | 'evening';

export function greetingFor(hour: number): Greeting {
  if (hour < 12) return 'morning';
  if (hour < 18) return 'afternoon';
  return 'evening';
}
