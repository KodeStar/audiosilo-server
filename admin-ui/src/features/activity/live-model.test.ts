import type { ListeningSession } from '@/api/types';
import type { TFunction } from 'i18next';
import { chapterLabel, liveSummary, sortLive } from './live-model';

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

  it('names the chapter by its title, else by its place, else not at all', () => {
    const t = ((key: string, o: { n: number }) => `${key}:${o.n}`) as unknown as TFunction;
    expect(chapterLabel({ chapter: 'The Wedding', chapter_index: 4 }, t)).toBe('The Wedding');
    expect(chapterLabel({ chapter_index: 23 }, t)).toBe('live.chapter:24');
    expect(chapterLabel({ chapter_index: 0 }, t)).toBe('live.chapter:1');
    expect(chapterLabel({}, t)).toBe('');
    expect(chapterLabel({ chapter: '', chapter_index: 2 }, t, 'activity.dropOff.chapter')).toBe(
      'activity.dropOff.chapter:3',
    );
  });
});
