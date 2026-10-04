import { ADMIN_BOOK_SORTS, type AdminBookSort } from '@/api/types';

// The Library destination's search params (every view deep-links, STYLEGUIDE.md
// section 2): the Books list's search, sort, view and facet filters, the exact
// author/series/narrator filters the Authors/Series/Narrators tiles open, and
// the Folders tree's selection. Validated here; router.tsx merges them into the
// destination routes' search.

export const LENGTHS = ['short', 'medium', 'long', 'epic'] as const;
export const ADDED = ['7', '30', '365'] as const;
export const YES_NO = ['yes', 'no'] as const;
export const PLAYBACK = ['direct', 'transcode'] as const;

export type Length = (typeof LENGTHS)[number];
export type Added = (typeof ADDED)[number];
export type YesNo = (typeof YES_NO)[number];
export type Playback = (typeof PLAYBACK)[number];

export interface LibrarySearch {
  /** Books: full-text filter. */
  q?: string;
  sort?: AdminBookSort;
  order?: 'asc' | 'desc';
  /** Books: the table view (the cover grid is the default). */
  view?: 'table';
  /** Books, Authors, Series, Narrators, Folders: one library (absent = all). */
  library?: number;
  /** Books: exact effective values (from an author tile, a series card). */
  author?: string;
  series?: string;
  narrator?: string;
  /** Books: facet filters. */
  format?: string[];
  codec?: string[];
  playback?: Playback;
  cover?: YesNo;
  chapters?: YesNo;
  matched?: YesNo;
  edited?: YesNo;
  length?: Length;
  added?: Added;
  /** Folders: the selected folder's library-relative path. */
  folder?: string;
}

/** Changes the Library search params from the previous ones (useUpdateSearch writes them). */
export type Update = (fn: (prev: LibrarySearch) => LibrarySearch) => void;

const oneOf = <T extends string>(allowed: readonly T[], v: unknown): T | undefined =>
  allowed.includes(v as T) ? (v as T) : undefined;

const text = (v: unknown): string | undefined =>
  typeof v === 'string' && v.trim() ? v : typeof v === 'number' ? String(v) : undefined;

const list = (v: unknown): string[] | undefined => {
  const items = (Array.isArray(v) ? v : [v]).map(text).filter((x): x is string => !!x);
  return items.length ? items : undefined;
};

/** Keeps the Library params that parse; anything else is dropped. */
export function validateLibrarySearch(s: Record<string, unknown>): LibrarySearch {
  const library = Number(s.library);
  const out: LibrarySearch = {
    q: text(s.q),
    sort: oneOf(ADMIN_BOOK_SORTS, s.sort),
    order: oneOf(['asc', 'desc'] as const, s.order),
    view: s.view === 'table' ? 'table' : undefined,
    library: Number.isInteger(library) && library > 0 ? library : undefined,
    author: text(s.author),
    series: text(s.series),
    narrator: text(s.narrator),
    format: list(s.format),
    codec: list(s.codec),
    playback: oneOf(PLAYBACK, s.playback),
    cover: oneOf(YES_NO, s.cover),
    chapters: oneOf(YES_NO, s.chapters),
    matched: oneOf(YES_NO, s.matched),
    edited: oneOf(YES_NO, s.edited),
    length: oneOf(LENGTHS, s.length),
    // A hand-written ?added=7 parses as a number; the app writes the string.
    added: oneOf(ADDED, text(s.added)),
    folder: typeof s.folder === 'string' ? s.folder : undefined,
  };
  for (const k of Object.keys(out) as (keyof LibrarySearch)[]) {
    if (out[k] === undefined) delete out[k];
  }
  return out;
}
