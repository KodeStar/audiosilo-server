import { describe, expect, it } from 'vitest';
import type { AdminLibrary, AdminModule, AdminUser } from '@/api/types';
import { bookRootOverlap, everyoneToGrant, podcastsHintKey } from './library-access';

const user = (u: Partial<AdminUser>): AdminUser =>
  ({ id: 1, username: 'u', role: 'user', disabled: false, ...u }) as AdminUser;

describe('everyoneToGrant', () => {
  it('grants every enabled listener', () => {
    const users = [user({ id: 1 }), user({ id: 2 })];
    expect(everyoneToGrant(users).map((u) => u.id)).toEqual([1, 2]);
  });
  it('leaves out admins, disabled accounts, household members and the demo account', () => {
    const users = [
      user({ id: 1, role: 'admin' }),
      user({ id: 2, disabled: true }),
      user({ id: 3, managed_by: { id: 9, username: 'o' } } as Partial<AdminUser>),
      user({ id: 4, is_demo: true }),
      user({ id: 5 }),
    ];
    expect(everyoneToGrant(users).map((u) => u.id)).toEqual([5]);
  });
});

describe('podcastsHintKey', () => {
  const mod = (m: Partial<AdminModule>): AdminModule =>
    ({ id: 'podcasts', enabled: false, active: false, bridge_state: 'ready', ...m }) as AdminModule;
  it('names what a podcast library still needs', () => {
    expect(podcastsHintKey(mod({ enabled: true }))).toBe('libraries.form.kindHintReady');
    expect(podcastsHintKey(mod({ enabled: true, bridge_state: 'offline' }))).toBe(
      'libraries.form.kindHintBridge',
    );
    expect(podcastsHintKey(mod({}))).toBe('libraries.form.kindHintModule');
    expect(podcastsHintKey(mod({ bridge_state: 'no_feature' }))).toBe('libraries.form.kindHint');
  });
  it('falls back to the general hint before the modules load', () => {
    expect(podcastsHintKey(undefined)).toBe('libraries.form.kindHint');
  });
});

describe('bookRootOverlap', () => {
  const lib = (id: number, name: string, root: string, kind: AdminLibrary['kind'] = 'books') => ({
    id,
    name,
    root,
    kind,
  });
  const libs = [
    lib(1, 'Fiction', '/mnt/books/fiction/'),
    lib(2, 'Podcasts', '/mnt/podcasts', 'podcasts'),
  ];

  it('finds a books library at the folder, holding it, or inside it', () => {
    expect(bookRootOverlap('/mnt/books/fiction', libs)).toEqual({ name: 'Fiction', how: 'same' });
    expect(bookRootOverlap('/mnt/books/fiction/Pratchett', libs)).toEqual({
      name: 'Fiction',
      how: 'inside',
    });
    expect(bookRootOverlap('/mnt/books/', libs)).toEqual({ name: 'Fiction', how: 'holds' });
    expect(bookRootOverlap('/', libs)).toEqual({ name: 'Fiction', how: 'holds' });
    expect(bookRootOverlap('/mnt//books/fiction', libs)).toEqual({ name: 'Fiction', how: 'same' });
  });

  it('leaves out a sibling, a longer name, podcast libraries and the library being edited', () => {
    expect(bookRootOverlap('/mnt/books/nonfiction', libs)).toBeUndefined();
    expect(bookRootOverlap('/mnt/books/fiction2', libs)).toBeUndefined();
    expect(bookRootOverlap('/mnt/podcasts/books', libs)).toBeUndefined();
    expect(bookRootOverlap('/mnt/books/fiction/x', libs, 1)).toBeUndefined();
    expect(bookRootOverlap('  ', libs)).toBeUndefined();
  });

  it('reads Windows paths with their separator', () => {
    const win = [lib(3, 'Books', 'D:\\Audio\\Books')];
    expect(bookRootOverlap('D:\\Audio\\Books\\Kids', win)).toEqual({
      name: 'Books',
      how: 'inside',
    });
    expect(bookRootOverlap('D:\\', win)).toEqual({ name: 'Books', how: 'holds' });
    // Typed with forward slashes: the server stores it with its own.
    expect(bookRootOverlap('D:/Audio/Books/Kids', win)).toEqual({ name: 'Books', how: 'inside' });
    expect(bookRootOverlap('D:\\Audio\\Books2', win)).toBeUndefined();
  });
});
