import type { FsEntry, FsListing } from '@/api/types';
import { choiceOf, modeOf } from './folder-modes';
import {
  ancestorsOf,
  audioFilesOf,
  entryIn,
  formatBytes,
  formatDuration,
  fullPath,
  parentOf,
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
  it('finds parents and ancestors', () => {
    expect(parentOf('A/B/C')).toBe('A/B');
    expect(parentOf('A')).toBe('');
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

  it('joins the library root and a relative path', () => {
    expect(fullPath('/mnt/tank/fiction', 'A/B')).toBe('/mnt/tank/fiction/A/B');
    expect(fullPath('/mnt/tank/fiction/', 'A')).toBe('/mnt/tank/fiction/A');
    expect(fullPath('/mnt/x', '')).toBe('/mnt/x');
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

describe('formatting', () => {
  it('formats sizes in decimal units', () => {
    expect(formatBytes(512, 'en')).toBe('0.5 kB');
    expect(formatBytes(1_310_000_000, 'en')).toBe('1.3 GB');
    expect(formatBytes(245_000_000, 'en')).toBe('245 MB');
  });

  it('formats lengths as hours and minutes', () => {
    expect(formatDuration(4530, 'en')).toBe('1h 15m');
    expect(formatDuration(3600, 'en')).toBe('1h');
    expect(formatDuration(600, 'en')).toBe('10m');
    expect(formatDuration(42, 'en')).toBe('42s');
  });
});
