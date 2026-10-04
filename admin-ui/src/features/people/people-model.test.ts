import type { AuthCode, ListeningRow, ListeningSession, Share } from '@/api/types';
import {
  currentBook,
  dateInputValue,
  datesEdit,
  datesProblem,
  inviteStatus,
  parseAccessChoice,
  ruleLabel,
  shareLabel,
  accessValue,
  sortInvites,
  wholeLibraryOf,
} from './people-model';

const now = Date.parse('2026-10-03T12:00:00Z');
const code = (over: Partial<AuthCode>): AuthCode => ({
  id: 1,
  label: 'invite',
  max_uses: 5,
  uses: 0,
  created_at: '2026-10-01T12:00:00Z',
  ...over,
});

describe('inviteStatus', () => {
  it('stays active until it expires or runs out of uses', () => {
    expect(inviteStatus(code({}), now)).toBe('active');
    expect(inviteStatus(code({ uses: 3 }), now)).toBe('active');
    expect(inviteStatus(code({ max_uses: 0, uses: 40 }), now)).toBe('active'); // unlimited
    expect(inviteStatus(code({ uses: 5 }), now)).toBe('usedUp');
    expect(inviteStatus(code({ expires_at: '2026-10-03T11:00:00Z' }), now)).toBe('expired');
    expect(inviteStatus(code({ expires_at: '2026-10-04T11:00:00Z' }), now)).toBe('active');
  });

  it('sorts active invites first, newest first', () => {
    const sorted = sortInvites(
      [
        code({ id: 1, uses: 5, created_at: '2026-10-03T00:00:00Z' }),
        code({ id: 2, created_at: '2026-09-01T00:00:00Z' }),
        code({ id: 3, created_at: '2026-10-02T00:00:00Z' }),
      ],
      now,
    );
    expect(sorted.map((c) => c.id)).toEqual([3, 2, 1]);
  });
});

describe('access', () => {
  const libs = [
    { id: 1, name: 'Fiction', root: '/f', default_view: '', sort_order: 0 },
    { id: 2, name: 'Kids', root: '/k', default_view: '', sort_order: 1 },
  ];
  const whole: Share = {
    id: 9,
    name: 'Library: Kids',
    description: 'Whole library',
    read_only: false,
    paths: [{ library_id: 2, path: '' }],
    whole_library_id: 2,
  };
  // An admin's own share whose only folder is a whole library is still a share.
  const ownWhole: Share = { ...whole, id: 10, name: 'Everything', whole_library_id: undefined };
  const named: Share = {
    id: 4,
    name: 'Cosy mysteries',
    description: '',
    read_only: false,
    paths: [
      { library_id: 1, path: 'Christie' },
      { library_id: 2, path: '' },
    ],
  };

  it('recognizes whole-library grants by the server mark, not their shape', () => {
    expect(wholeLibraryOf(whole)).toBe(2);
    expect(wholeLibraryOf(ownWhole)).toBeUndefined();
    expect(wholeLibraryOf(named)).toBeUndefined();
    expect(wholeLibraryOf(named)).toBeUndefined();
  });

  it('names whole-library grants after their library', () => {
    expect(shareLabel(whole, libs)).toBe('Kids');
    expect(shareLabel(named, libs)).toBe('Cosy mysteries');
    expect(shareLabel(whole, [])).toBe('Library: Kids');
  });

  it('round-trips access choices', () => {
    expect(parseAccessChoice(accessValue({ kind: 'share', id: 7 }))).toEqual({
      kind: 'share',
      id: 7,
    });
    expect(parseAccessChoice('library:2')).toEqual({ kind: 'library', id: 2 });
    expect(parseAccessChoice('all')).toBeUndefined();
    expect(parseAccessChoice('share:x')).toBeUndefined();
  });

  it('labels rules by library', () => {
    expect(ruleLabel({ library_id: 1, path: 'Christie' }, libs)).toBe('Fiction › Christie');
    expect(ruleLabel({ library_id: 2, path: '' }, libs)).toBe('Kids');
    expect(ruleLabel({ library_id: 7, path: '' }, libs)).toBe('#7');
  });
});

describe('currentBook', () => {
  const row = (over: Partial<ListeningRow>): ListeningRow => ({
    user_id: 2,
    username: 'sam',
    library_id: 1,
    path: 'a',
    title: 'A',
    author: '',
    position: 10,
    duration: 100,
    finished: false,
    updated_at: '2026-10-03T11:55:00Z',
    ...over,
  });

  const rows = [
    row({ path: 'old', updated_at: '2026-09-01T00:00:00Z' }),
    row({ path: 'done', finished: true, updated_at: '2026-10-03T11:59:00Z' }),
    row({ path: 'newest' }),
    row({ user_id: 3, path: 'someone else', updated_at: '2026-10-03T11:59:30Z' }),
  ];

  it("picks the person's newest unfinished book when nothing is live", () => {
    expect(currentBook(rows, [], 2)).toMatchObject({ path: 'newest', live: false });
    expect(currentBook(rows, [], 99)).toBeUndefined();
  });

  it('prefers what they are playing now', () => {
    const live = (over: Partial<ListeningSession>) =>
      ({
        ...row({}),
        state: 'playing',
        last_at: '2026-10-03T11:59:00Z',
        ...over,
      }) as ListeningSession;
    const sessions = [
      live({ path: 'paused', state: 'paused', last_at: '2026-10-03T11:59:50Z' }),
      live({ path: 'playing', position: 42 }),
      live({ user_id: 3, path: 'not theirs' }),
    ];
    expect(currentBook(rows, sessions, 2)).toMatchObject({
      path: 'playing',
      position: 42,
      live: true,
    });
  });
});

describe('progress dates', () => {
  it('reads a moment as the browser day a date input shows', () => {
    const d = new Date(2026, 8, 1, 9, 30);
    expect(dateInputValue(d.toISOString())).toBe('2026-09-01');
    expect(dateInputValue(null)).toBe('');
  });

  it('refuses future dates and a finish before the start', () => {
    expect(datesProblem('2026-09-01', '2026-09-20', '2026-10-04')).toBeUndefined();
    expect(datesProblem('2026-09-01', '', '2026-10-04')).toBeUndefined();
    expect(datesProblem('2026-10-05', '', '2026-10-04')).toBe('progress.dates.future');
    expect(datesProblem('2026-09-20', '2026-09-01', '2026-10-04')).toBe('progress.dates.order');
  });

  it('sends only the dates that changed, null to clear', () => {
    const before = { started: '2026-09-01', finished: '2026-09-20' };
    expect(datesEdit(before, before)).toEqual({});
    expect(datesEdit(before, { started: '', finished: '2026-09-21' })).toEqual({
      started_at: null,
      finished_at: '2026-09-21',
    });
  });
});
