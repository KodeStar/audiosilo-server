import type { FsEntry, FsListing } from '@/api/types';
import { choiceOf, modeOf } from './folder-modes';
import {
  ancestorsOf,
  audioFilesOf,
  entryIn,
  joinChoices,
  offersBook,
  offersCollection,
  treeKeyAction,
  treeRows,
  visibleListings,
  withAncestors,
  type ListingState,
  type TreeRow,
} from './folders-model';

const dir = (path: string, over: Partial<FsEntry> = {}): FsEntry => ({
  name: path.split('/').pop()!,
  path,
  is_dir: true,
  is_audio: false,
  size: 0,
  mod_time: 0,
  ...over,
});
const file = (path: string, size = 100, duration?: number): FsEntry => ({
  name: path.split('/').pop()!,
  path,
  is_dir: false,
  is_audio: true,
  size,
  mod_time: 0,
  duration,
});
const listing = (path: string, entries: FsEntry[], over: Partial<FsListing> = {}): FsListing => ({
  path,
  entries,
  total: entries.length,
  offset: 0,
  ...over,
});

// Fiction/
//   Sanderson/        (Mistborn/ with 3 files, Elantris/ with 1)
//   Pratchett/
//   loose.mp3
const FS: Record<string, FsListing> = {
  '': listing('', [dir('Sanderson'), dir('Pratchett'), file('loose.mp3')]),
  Sanderson: listing('Sanderson', [
    dir('Sanderson/Mistborn', { is_book: true }),
    dir('Sanderson/Elantris'),
  ]),
  'Sanderson/Mistborn': listing('Sanderson/Mistborn', [
    file('Sanderson/Mistborn/1.m4b', 10, 60),
    file('Sanderson/Mistborn/2.m4b', 20, 120),
  ]),
};
const loaded = (p: string): ListingState | undefined => (FS[p] ? { data: FS[p] } : undefined);
const paths = (rows: TreeRow[]) =>
  rows.map((r) => (r.kind === 'folder' ? r.entry.path : `${r.kind}:${r.parent}`));

describe('paths', () => {
  it('finds ancestors', () => {
    expect(ancestorsOf('A/B/C')).toEqual(['A', 'A/B']);
    expect(ancestorsOf('A')).toEqual([]);
  });

  it('opens the ancestors of a deep link, keeping the set when nothing changes', () => {
    const open = new Set(['X']);
    expect([...withAncestors(open, 'A/B/C')]).toEqual(['X', 'A', 'A/B']);
    expect(withAncestors(open, 'X/Y')).toBe(open);
  });

  it('loads only the listings that show', () => {
    // A/B is open but A is closed: B's listing isn't visible.
    expect(visibleListings(new Set(['S', 'A/B']))).toEqual(['', 'S']);
  });
});

describe('treeRows', () => {
  it('lists the root folders, not its audio files', () => {
    const rows = treeRows(loaded, new Set());
    expect(paths(rows)).toEqual(['Sanderson', 'Pratchett']);
    expect(rows[0]).toMatchObject({ level: 1, posInSet: 1, setSize: 2, expanded: false });
  });

  it('nests open folders depth first, with a loading row while one loads', () => {
    const rows = treeRows(loaded, new Set(['Sanderson', 'Sanderson/Elantris']));
    expect(paths(rows)).toEqual([
      'Sanderson',
      'Sanderson/Mistborn',
      'Sanderson/Elantris',
      'loading:Sanderson/Elantris',
      'Pratchett',
    ]);
    expect(rows[1]).toMatchObject({ level: 2, posInSet: 1, setSize: 2 });
  });

  it('marks a folder with no subfolders as a leaf once its listing is known', () => {
    const rows = treeRows(loaded, new Set(['Sanderson', 'Sanderson/Mistborn']));
    const mistborn = rows.find((r) => r.kind === 'folder' && r.entry.path === 'Sanderson/Mistborn');
    expect(mistborn).toMatchObject({ expandable: false, expanded: false });
    const elantris = rows.find((r) => r.kind === 'folder' && r.entry.path === 'Sanderson/Elantris');
    expect(elantris).toMatchObject({ expandable: true });
  });

  it('shows an error row for a listing that failed, and a note for a cut-short one', () => {
    const state = (p: string): ListingState | undefined =>
      p === 'Sanderson'
        ? { failed: true }
        : p === ''
          ? { data: { ...FS[''], next_offset: 500 } }
          : undefined;
    expect(paths(treeRows(state, new Set(['Sanderson'])))).toEqual([
      'Sanderson',
      'error:Sanderson',
      'Pratchett',
      'more:',
    ]);
  });
});

describe('treeKeyAction', () => {
  const rows = treeRows(loaded, new Set(['Sanderson']));

  it('moves through the visible folders', () => {
    expect(treeKeyAction(rows, 'Sanderson', 'ArrowDown')).toEqual({
      type: 'focus',
      path: 'Sanderson/Mistborn',
    });
    expect(treeKeyAction(rows, 'Sanderson', 'ArrowUp')).toBeNull();
    expect(treeKeyAction(rows, 'Sanderson/Elantris', 'Home')).toEqual({
      type: 'focus',
      path: 'Sanderson',
    });
    expect(treeKeyAction(rows, 'Sanderson', 'End')).toEqual({ type: 'focus', path: 'Pratchett' });
  });

  it('opens, enters, closes and leaves folders', () => {
    expect(treeKeyAction(rows, 'Pratchett', 'ArrowRight')).toEqual({
      type: 'expand',
      path: 'Pratchett',
    });
    expect(treeKeyAction(rows, 'Sanderson', 'ArrowRight')).toEqual({
      type: 'focus',
      path: 'Sanderson/Mistborn',
    });
    expect(treeKeyAction(rows, 'Sanderson', 'ArrowLeft')).toEqual({
      type: 'collapse',
      path: 'Sanderson',
    });
    expect(treeKeyAction(rows, 'Sanderson/Mistborn', 'ArrowLeft')).toEqual({
      type: 'focus',
      path: 'Sanderson',
    });
    expect(treeKeyAction(rows, 'Pratchett', 'ArrowLeft')).toBeNull();
  });

  it('selects with Enter and Space, and ignores other keys', () => {
    expect(treeKeyAction(rows, 'Pratchett', 'Enter')).toEqual({
      type: 'select',
      path: 'Pratchett',
    });
    expect(treeKeyAction(rows, 'Pratchett', ' ')).toEqual({ type: 'select', path: 'Pratchett' });
    expect(treeKeyAction(rows, 'Pratchett', 'a')).toBeNull();
  });
});

describe('a folder', () => {
  it('finds its own entry in its parent listing', () => {
    expect(entryIn(FS.Sanderson, 'Sanderson/Mistborn')?.is_book).toBe(true);
    expect(entryIn(FS.Sanderson, 'Sanderson/Gone')).toBeUndefined();
  });

  it('counts its audio files, with a total length only when every file knows it', () => {
    expect(audioFilesOf(FS['Sanderson/Mistborn'])).toMatchObject({ size: 30, duration: 180 });
    expect(audioFilesOf(FS['Sanderson/Mistborn']).files).toHaveLength(2);
    const partial = listing('x', [file('x/a.mp3', 1, 10), file('x/b.mp3', 1)]);
    expect(audioFilesOf(partial).duration).toBeUndefined();
    expect(audioFilesOf(FS.Sanderson).files).toEqual([]);
  });

  it('maps its override to a choice and back', () => {
    expect(choiceOf(undefined)).toBe('auto');
    expect(choiceOf('collection')).toBe('collection');
    expect(modeOf('auto')).toBeNull();
    expect(modeOf('book')).toBe('book');
  });
});

it('offers the choices that mean something for a folder', () => {
  // Audio of its own: every choice.
  expect(joinChoices(3, {}, [])).toEqual({
    enabled: ['auto', 'book', 'collection'],
    joinable: false,
    subBooks: 0,
  });
  // Audio only in its disc folders: joined into one book, or each its own; nothing to split.
  expect(
    joinChoices(0, { split_discs: true }, [
      dir('B/CD1', { is_book: true }),
      dir('B/CD2', { is_book: true }),
      dir('B/Art'),
    ]),
  ).toEqual({
    enabled: ['auto', 'book'],
    joinable: true,
    subBooks: 2,
  });
  // Already joined (its discs are no books of their own any more).
  expect(joinChoices(0, { override: 'book', is_book: true }, [])).toMatchObject({
    enabled: ['auto', 'book'],
    joinable: true,
  });
  // A folder of books that aren't discs (an author's, a series'): the server joins
  // nothing there, so only clearing an override it carries (one set before).
  const books = [dir('A/One', { is_book: true }), dir('A/Two', { is_book: true })];
  expect(joinChoices(0, {}, books).enabled).toEqual([]);
  expect(joinChoices(0, { override: 'book' }, books)).toMatchObject({
    enabled: ['auto'],
    joinable: false,
  });
  expect(joinChoices(0, { override: 'collection' }, []).enabled).toEqual(['auto']);
});

it('offers "Always one book" in the detection dialog where it means something', () => {
  expect(offersBook({ is_book: true })).toBe(true); // a book (its own audio, or joined)
  expect(offersBook({ split_discs: true })).toBe(true); // discs to join
  expect(offersBook({ override: 'collection' })).toBe(true); // its own audio, split
  expect(offersBook({ override: 'book' })).toBe(true); // what it is set to
  expect(offersBook({})).toBe(false); // a folder of books, or of nothing
});

it('offers "Separate books" in the detection dialog except on discs to join', () => {
  expect(offersCollection({})).toBe(true);
  expect(offersCollection({ split_discs: true })).toBe(false); // no files of its own to split
  expect(offersCollection({ split_discs: true, override: 'collection' })).toBe(true); // as set
});
