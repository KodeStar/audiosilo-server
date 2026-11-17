import type { LibraryStat, ListeningRow, ListeningSession } from '@/api/types';
import { liveSummary, compareLive } from '@/features/activity/live-model';
import { refKey } from '@/lib/book-route';
import { isBookLibrary } from '@/lib/library-kind';

/** The recent-listening list on Overview shows at most this many rows. */
export const RECENT_LIMIT = 8;

/** One person on one book right now: the newest of their live sessions on it, and on how many devices. */
export interface LiveListening {
  /** The person and book: a stable key while the newest session changes. */
  id: string;
  session: ListeningSession;
  devices: number;
}

export interface ListeningSplit {
  /** Who is listening to what right now, one card per person and book, playing first. */
  live: LiveListening[];
  /** Distinct people among `live` (one person can listen on two devices). */
  listeners: number;
  /** The newest progress that isn't live right now. */
  recent: ListeningRow[];
  inProgress: number;
}

const key = (r: { user_id: number; library_id: number; path: string }) =>
  `${r.user_id}\0${refKey(r)}`;

/**
 * The live sessions one card per person and book (server#186): a book playing on
 * a phone and a speaker is one card, its state the newest session's, saying on
 * how many devices.
 */
function groupLive(live: readonly ListeningSession[]): LiveListening[] {
  const groups = new Map<string, { session: ListeningSession; devices: Set<number> }>();
  for (const s of live) {
    const k = key(s);
    const g = groups.get(k);
    if (!g) groups.set(k, { session: s, devices: new Set([s.device_id]) });
    else {
      g.devices.add(s.device_id);
      if (Date.parse(s.last_at) > Date.parse(g.session.last_at)) g.session = s;
    }
  }
  const cards = [...groups].map(([id, g]) => ({ id, session: g.session, devices: g.devices.size }));
  return cards.sort((a, b) => compareLive(a.session, b.session));
}

/**
 * Splits the Overview's listening into who is listening right now (the server's
 * live sessions, one card per person and book) and the recent history (the
 * admin progress feed, newest first, without the books being listened to now).
 */
export function splitListening(
  rows: readonly ListeningRow[],
  live: readonly ListeningSession[],
): ListeningSplit {
  const sorted = [...rows].sort((a, b) => Date.parse(b.updated_at) - Date.parse(a.updated_at));
  const now = new Set(live.map(key));
  return {
    live: groupLive(live),
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

/** The libraries "Books per library" lists: books libraries (a podcast library has no books). */
export function bookLibraries(libraries: readonly LibraryStat[]): LibraryStat[] {
  return libraries.filter(isBookLibrary);
}
