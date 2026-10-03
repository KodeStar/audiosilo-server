import type { AuthCode, ListeningRow, Share } from '@/api/types';
import {
  currentBook,
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

  it("picks the person's newest unfinished book and says whether it's live", () => {
    const rows = [
      row({ path: 'old', updated_at: '2026-09-01T00:00:00Z' }),
      row({ path: 'done', finished: true, updated_at: '2026-10-03T11:59:00Z' }),
      row({ path: 'now' }),
      row({ user_id: 3, path: 'someone else', updated_at: '2026-10-03T11:59:30Z' }),
    ];
    expect(currentBook(rows, 2, now)).toEqual({ row: rows[2], live: true });
    expect(currentBook([rows[0]], 2, now)?.live).toBe(false);
    expect(currentBook(rows, 99, now)).toBeUndefined();
  });
});
