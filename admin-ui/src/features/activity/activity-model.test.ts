import type { ActivityDay, ListeningSession, UserProgress } from '@/api/types';
import {
  busiestSlot,
  calendarGrid,
  change,
  clientKind,
  clientParts,
  clientRows,
  compareVersions,
  finishedIn,
  finishRate,
  hoursBars,
  liveSummary,
  longestStreak,
  monthTotals,
  niceTicks,
  playbackParts,
  recentYears,
  seqLevel,
  sortLive,
  topListeners,
  weekdayName,
  weekdayOf,
} from './activity-model';

const H = 3600;

function day(date: string, byUser: Record<number, number | undefined> = {}): ActivityDay {
  const by_user = Object.entries(byUser).map(([id, h]) => ({
    user_id: Number(id),
    listened: (h ?? 0) * H,
  }));
  return { date, listened: by_user.reduce((s, u) => s + u.listened, 0), by_user };
}

/** `n` consecutive days from `start`, each with `byUser(i)`. */
function days(
  start: string,
  n: number,
  byUser: (i: number) => Record<number, number | undefined> = () => ({}),
) {
  const t0 = Date.parse(`${start}T00:00:00Z`);
  return Array.from({ length: n }, (_, i) =>
    day(new Date(t0 + i * 86_400_000).toISOString().slice(0, 10), byUser(i)),
  );
}

describe('change', () => {
  it('is the fraction against the previous period, null with nothing before', () => {
    expect(change(112, 100)).toBeCloseTo(0.12);
    expect(change(50, 100)).toBeCloseTo(-0.5);
    expect(change(10, 0)).toBeNull();
  });
});

describe('weekdayOf', () => {
  it('reads the server day as a calendar date, Monday first', () => {
    expect(weekdayOf('2026-10-05')).toBe(0); // a Monday
    expect(weekdayOf('2026-10-04')).toBe(6); // a Sunday
    expect(weekdayName(0, 'en')).toBe('Mon');
    expect(weekdayName(6, 'en', 'long')).toBe('Sunday');
  });
});

describe('topListeners + hoursBars', () => {
  const period = [day('2026-10-01', { 1: 2, 2: 1, 3: 0.5 }), day('2026-10-02', { 2: 4 })];

  it('stacks the biggest listeners and folds the rest into others', () => {
    const top = topListeners(period, 2);
    expect(top).toEqual([2, 1]);
    const bars = hoursBars(period, top);
    expect(bars).toHaveLength(2);
    expect(bars[0]).toEqual({
      from: '2026-10-01',
      to: '2026-10-01',
      series: [1, 2, 0.5],
      total: 3.5,
    });
    expect(bars[1].series).toEqual([4, 0, 0]);
  });

  it('draws a bar a week once the period is longer than a quarter', () => {
    const year = days('2025-10-05', 365, (i) => (i === 0 || i === 8 ? { 1: 1 } : {}));
    const bars = hoursBars(year, [1]);
    expect(bars).toHaveLength(53); // 52 full weeks and one day
    expect(bars[0]).toMatchObject({ from: '2025-10-05', to: '2025-10-11', total: 1 });
    expect(bars[1].series[0]).toBe(1);
    expect(bars[52].from).toBe(bars[52].to);
  });

  it('ignores listeners with no time', () => {
    expect(topListeners([day('2026-10-01', { 4: 0 })])).toEqual([]);
  });
});

describe('niceTicks', () => {
  it('steps by 1, 2 or 5 times a power of ten from zero', () => {
    expect(niceTicks(2.6)).toEqual([0, 1, 2, 3]);
    expect(niceTicks(7.3)).toEqual([0, 2, 4, 6, 8]);
    expect(niceTicks(0.4)).toEqual([0, 0.1, 0.2, 0.3, 0.4]);
    expect(niceTicks(120)).toEqual([0, 50, 100, 150]);
    expect(niceTicks(0)).toEqual([0, 1]);
  });
});

describe('seqLevel', () => {
  it('maps a value to the six-step scale, 0 only for nothing', () => {
    expect(seqLevel(0, 10)).toBe(0);
    expect(seqLevel(0.01, 10)).toBe(1);
    expect(seqLevel(5, 10)).toBe(3);
    expect(seqLevel(10, 10)).toBe(5);
    expect(seqLevel(3, 0)).toBe(0);
  });
});

describe('calendarGrid', () => {
  it('lays days out in Monday-first weeks with month labels', () => {
    const { weeks, months, max } = calendarGrid(
      days('2026-09-03', 40, (i) => (i === 5 ? { 1: 2 } : {})),
    );
    // 2026-09-03 is a Thursday: three blanks first.
    expect(weeks[0].slice(0, 3)).toEqual([null, null, null]);
    expect(weeks[0][3]?.date).toBe('2026-09-03');
    expect(weeks.every((w) => w.length <= 7)).toBe(true);
    expect(max).toBe(2 * H);
    expect(weeks.flat().find((c) => c?.date === '2026-09-08')?.level).toBe(5);
    expect(months.map((m) => m.date)).toEqual(['2026-09-03', '2026-10-01']);
  });

  it('leaves out a closing label with no room before the end', () => {
    const { months } = calendarGrid(days('2026-08-01', 62));
    expect(months.map((m) => m.date)).toEqual(['2026-08-01', '2026-09-01']);
  });

  it('leaves out an opening label with no room before the next month', () => {
    const { months } = calendarGrid(days('2026-09-25', 30));
    expect(months.map((m) => m.date)).toEqual(['2026-10-01']);
  });
});

describe('busiestSlot', () => {
  it('finds the busiest hour of the week', () => {
    const grid = Array.from({ length: 7 }, () => new Array<number>(24).fill(0));
    grid[5][21] = 900;
    grid[0][8] = 300;
    expect(busiestSlot(grid)).toEqual({ weekday: 5, hour: 21, listened: 900 });
    expect(busiestSlot([new Array<number>(24).fill(0)])).toBeNull();
  });
});

describe('finishRate', () => {
  it('is finished over started', () => {
    const f = { started: 4, reached_25: 3, reached_50: 2, reached_75: 2, finished: 1 };
    expect(finishRate(f)).toBe(0.25);
    expect(finishRate({ ...f, started: 0 })).toBeNull();
  });
});

describe('streaks and months', () => {
  const period = days('2026-01-30', 5, (i) =>
    i === 2 ? {} : { 1: 1, ...(i < 2 ? { 2: 1 } : {}) },
  );

  it('finds the longest run of days with listening, for everyone or one person', () => {
    expect(longestStreak(period)).toBe(2);
    expect(longestStreak(period, 2)).toBe(2);
    expect(longestStreak(period, 9)).toBe(0);
  });

  it('sums by calendar month', () => {
    const m = monthTotals(period, 1);
    expect(m[0]).toBe(2 * H); // Jan 30, 31
    expect(m[1]).toBe(2 * H); // Feb 2, 3 (Feb 1 had none)
    expect(m.slice(2).every((v) => v === 0)).toBe(true);
  });
});

describe('finishedIn', () => {
  const row = (path: string, finished_at: string | null, finished = true): UserProgress => ({
    library_id: 1,
    path,
    position: 0,
    duration: 0,
    finished,
    playback_speed: 1,
    version: 1,
    device_id: '',
    updated_at: '2026-01-01T00:00:00Z',
    title: path,
    author: '',
    started_at: null,
    finished_at,
  });

  it('keeps the books finished that year, newest first', () => {
    const rows = [
      row('a', '2026-02-01T10:00:00Z'),
      row('b', '2025-12-01T10:00:00Z'),
      row('c', '2026-06-01T10:00:00Z'),
      row('d', null),
      row('e', '2026-03-01T10:00:00Z', false),
    ];
    expect(finishedIn(rows, 2026).map((r) => r.path)).toEqual(['c', 'a']);
  });
});

describe('playbackParts', () => {
  it('puts direct play first and folds small transcodes into other', () => {
    const parts = playbackParts(
      [
        { transcoded: false, codec: 'aac', listened: 600, sessions: 3 },
        { transcoded: false, codec: 'mp3', listened: 300, sessions: 1 },
        { transcoded: true, codec: 'opus', listened: 60, sessions: 1 },
        { transcoded: true, codec: 'flac', listened: 30, sessions: 1 },
        { transcoded: true, codec: '', listened: 10, sessions: 1 },
      ],
      1,
    );
    expect(parts.map((p) => [p.key, p.share])).toEqual([
      ['direct', 0.9],
      ['opus', 0.06],
      ['other', 0.04],
    ]);
    expect(playbackParts([])).toEqual([]);
  });
});

describe('versions and clients', () => {
  it('compares dotted versions numerically', () => {
    expect(compareVersions('1.10.0', '1.9.2')).toBe(1);
    expect(compareVersions('1.4', '1.4.0')).toBe(0);
    expect(compareVersions('0.9', '1.0')).toBe(-1);
  });

  it('marks builds a newer build of the same app and platform replaces', () => {
    const rows = clientRows([
      { app: 'AudioSilo', version: '1.3.0', platform: 'ios', devices: 1 },
      { app: 'AudioSilo', version: '1.4.2', platform: 'ios', devices: 5 },
      { app: 'AudioSilo', version: '1.3.0', platform: 'android', devices: 2 },
      { app: '', version: '', platform: '', devices: 1 },
    ]);
    expect(rows.map((r) => [r.version, r.platform, r.outdated])).toEqual([
      ['1.4.2', 'ios', false],
      ['1.3.0', 'android', false],
      ['1.3.0', 'ios', true],
      ['', '', false],
    ]);
  });

  it('names a client', () => {
    expect(clientKind(null)).toBe('unknown');
    expect(clientKind({ app: '', platform: '' })).toBe('unknown');
    expect(clientKind({ app: 'AudioSilo Admin', platform: 'web' })).toBe('console');
    expect(clientKind({ app: 'AudioSilo', platform: 'iOS' })).toBe('phone');
    expect(clientKind({ app: 'AudioSilo', platform: 'web' })).toBe('web');
    expect(clientParts({ app: 'AudioSilo', version: '1.4.2', platform: 'android' })).toEqual({
      app: 'AudioSilo 1.4.2',
      platform: 'Android',
    });
    expect(clientParts({ app: 'Shelfie', version: '', platform: 'tv' })).toEqual({
      app: 'Shelfie',
      platform: 'tv',
    });
  });
});

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

describe('recentYears', () => {
  it('offers this year and four before it', () => {
    expect(recentYears(new Date(2026, 5, 1))).toEqual([2026, 2025, 2024, 2023, 2022]);
  });
});
