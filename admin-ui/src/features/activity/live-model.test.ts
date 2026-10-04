import type { ListeningSession } from '@/api/types';
import { liveSummary, sortLive } from './live-model';

describe('live sessions', () => {
  const s = (
    id: number,
    user: number,
    state: ListeningSession['state'],
    last: string,
    transcoded = false,
  ) => ({ id, user_id: user, state, last_at: last, transcoded }) as ListeningSession;

  it('lists playing first, newest first, and sums them up', () => {
    const list = [
      s(1, 1, 'paused', '2026-10-04T10:05:00Z'),
      s(2, 2, 'playing', '2026-10-04T10:00:00Z', true),
      s(3, 1, 'playing', '2026-10-04T10:04:00Z'),
    ];
    expect(sortLive(list).map((x) => x.id)).toEqual([3, 2, 1]);
    expect(liveSummary(list)).toEqual({ playing: 2, paused: 1, transcoding: 1, listeners: 2 });
  });
});
