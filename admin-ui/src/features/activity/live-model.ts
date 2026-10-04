import type { ListeningSession } from '@/api/types';

// Who is listening now, shared by the Overview, People and Activity > Live now (kept apart from
// activity-model so the Overview's entry chunk doesn't carry the Activity page's logic).

/** Live sessions in the order the Live page shows them: playing first, then the newest save. */
export function sortLive(sessions: readonly ListeningSession[]): ListeningSession[] {
  const rank = (s: ListeningSession) => (s.state === 'playing' ? 0 : 1);
  return [...sessions].sort(
    (a, b) => rank(a) - rank(b) || Date.parse(b.last_at) - Date.parse(a.last_at),
  );
}

/** A live list's headline numbers: streams, how many play direct, and distinct listeners. */
export function liveSummary(sessions: readonly ListeningSession[]) {
  const playing = sessions.filter((s) => s.state === 'playing');
  return {
    playing: playing.length,
    paused: sessions.length - playing.length,
    transcoding: playing.filter((s) => s.transcoded).length,
    listeners: new Set(sessions.map((s) => s.user_id)).size,
  };
}
