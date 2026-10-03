import type { BookFilter, BookListParams } from '@/api/client';
import type { AdminBook, AdminBookSort, BookRef, FacetCount } from '@/api/types';
import type { Length, LibrarySearch, YesNo } from '../library-search';

// Library > Books, as pure functions: the URL's search params (LibrarySearch)
// as the API's filter and page request, the active-filter chips, the
// selection key, the shelves' and tiles' issue flags, and the grid geometry
// the virtualizer needs. The components stay thin.

const HOUR = 3600;
const DAY_MS = 24 * 3600 * 1000;

/** How many books one keyset page asks for (the server caps it at 200). */
export const PAGE_SIZE = 60;
/** The server's cap on one bulk edit (handlers_catalog.go), applied all or nothing. */
export const BULK_LIMIT = 1000;
/** How many books a shelf shows. */
export const SHELF_SIZE = 12;

/** The sort menu's orderings, in its order. */
export const SORT_OPTIONS: readonly AdminBookSort[] = [
  'title',
  'author',
  'series',
  'narrator',
  'added',
  'duration',
  'size',
];

/** Newest, longest and largest read best first; names alphabetically. */
export function defaultOrder(sort: AdminBookSort): 'asc' | 'desc' {
  return sort === 'added' || sort === 'duration' || sort === 'size' ? 'desc' : 'asc';
}

/** The length chips as duration bounds in seconds (inclusive, as the server compares). */
export const LENGTH_RANGES: Record<Length, Pick<BookFilter, 'min_duration' | 'max_duration'>> = {
  short: { max_duration: 5 * HOUR },
  medium: { min_duration: 5 * HOUR, max_duration: 15 * HOUR },
  long: { min_duration: 15 * HOUR, max_duration: 30 * HOUR },
  epic: { min_duration: 30 * HOUR },
};

/**
 * The UTC calendar date `days` before `now` (YYYY-MM-DD). added_at is stored as
 * RFC 3339 UTC and the server compares a bare date as its prefix, so the date is
 * taken in UTC too.
 */
export function daysAgo(days: number, now: number): string {
  return new Date(now - days * DAY_MS).toISOString().slice(0, 10);
}

/** An object without its unset values, so equal filters make equal query keys. */
function compact<T extends object>(o: T): T {
  return Object.fromEntries(
    Object.entries(o).filter(
      ([, v]) => v !== undefined && v !== '' && !(Array.isArray(v) && v.length === 0),
    ),
  ) as T;
}

const yes = (v: YesNo | undefined) => (v === undefined ? undefined : v === 'yes');

/** The URL's filters as the API's (shared by the list and the facet counts). */
export function bookFilter(s: LibrarySearch, now: number): BookFilter {
  return compact<BookFilter>({
    q: s.q?.trim(),
    library_id: s.library,
    author: s.author,
    series: s.series,
    narrator: s.narrator,
    format: s.format,
    codec: s.codec,
    direct_playable: s.playback === undefined ? undefined : s.playback === 'direct',
    has_cover: yes(s.cover),
    has_chapters: yes(s.chapters),
    matched: yes(s.matched),
    edited: yes(s.edited),
    ...(s.length ? LENGTH_RANGES[s.length] : {}),
    added_after: s.added ? daysAgo(Number(s.added), now) : undefined,
  });
}

/** One page request of the full list (the cursor is added per page). */
export function listParams(s: LibrarySearch, now: number): Omit<BookListParams, 'cursor'> {
  const sort = s.sort ?? 'title';
  return { ...bookFilter(s, now), sort, order: s.order ?? defaultOrder(sort), limit: PAGE_SIZE };
}

/** The search params a filter chip stands for, in the chips' order. */
export const FILTER_KEYS = [
  'library',
  'author',
  'series',
  'narrator',
  'format',
  'codec',
  'playback',
  'cover',
  'matched',
  'chapters',
  'edited',
  'length',
  'added',
] as const;
export type FilterKey = (typeof FILTER_KEYS)[number];

/** The exact-value filters other screens link with; they have no group in the sheet. */
const EXACT: ReadonlySet<FilterKey> = new Set(['author', 'series', 'narrator']);

/** One active filter (a chip): a key and one of its values. */
export interface ActiveFilter {
  key: FilterKey;
  value: string;
}

/** Every active filter, one per value of the repeatable ones (format, codec). */
export function activeFilters(s: LibrarySearch): ActiveFilter[] {
  const out: ActiveFilter[] = [];
  for (const key of FILTER_KEYS) {
    const v = s[key];
    if (v === undefined) continue;
    for (const value of Array.isArray(v) ? v : [String(v)]) out.push({ key, value });
  }
  return out;
}

/** How many filters the sheet has on (the badge on the Filters button). */
export function sheetFilterCount(s: LibrarySearch): number {
  return activeFilters(s).filter((f) => !EXACT.has(f.key)).length;
}

/** No search and no filter: the shelves show above the list. */
export function isBrowsing(s: LibrarySearch): boolean {
  return !s.q?.trim() && activeFilters(s).length === 0;
}

/** The search params without one chip's filter. */
export function withoutFilter(s: LibrarySearch, f: ActiveFilter): LibrarySearch {
  const v = s[f.key];
  if (Array.isArray(v)) {
    const rest = v.filter((x) => x !== f.value);
    return { ...s, [f.key]: rest.length ? rest : undefined };
  }
  return { ...s, [f.key]: undefined };
}

/** The search params with every filter cleared, and the search too unless `keepQuery`. */
export function withoutFilters(s: LibrarySearch, keepQuery = false): LibrarySearch {
  const out: LibrarySearch = { ...s };
  for (const key of FILTER_KEYS) delete out[key];
  if (!keepQuery) delete out.q;
  return out;
}

/** A repeatable filter with `value` toggled; undefined once it holds nothing. */
export function toggleValue(list: string[] | undefined, value: string): string[] | undefined {
  const next = list?.includes(value) ? list.filter((x) => x !== value) : [...(list ?? []), value];
  return next.length ? next : undefined;
}

/**
 * A repeatable facet's chips: the counted values, plus any selected value the
 * counts no longer hold (so it can still be turned off), with a zero count.
 */
export function facetOptions(counts: FacetCount[] | undefined, selected?: string[]) {
  const list = [...(counts ?? [])];
  for (const v of selected ?? []) {
    if (!list.some((c) => c.value === v)) list.push({ value: v, count: 0 });
  }
  return list;
}

/** Search params merged with a patch, dropping what the patch unsets. */
export function patchSearch<S extends LibrarySearch>(prev: S, patch: Partial<LibrarySearch>): S {
  return compact({ ...prev, ...patch });
}

/** A book's key in the selection (library + path is its identity, never an id). */
export const selectionKey = (b: BookRef) => `${b.library_id}\0${b.path}`;

export type BulkField = 'author' | 'narrator' | 'series';
export const BULK_FIELDS: readonly BulkField[] = ['author', 'narrator', 'series'];

/** The value all the books share for a field, or how many different values they hold. */
export function commonValue(
  books: AdminBook[],
  field: BulkField,
): { same: true; value: string } | { same: false; distinct: number } {
  const values = new Set(books.map((b) => b[field]));
  return values.size <= 1
    ? { same: true, value: books[0]?.[field] ?? '' }
    : { same: false, distinct: values.size };
}

/** The bulk edit's `set`: only the fields filled in, trimmed. */
export function bulkSet(
  values: Partial<Record<BulkField, string>>,
): Partial<Record<BulkField, string>> {
  const out: Partial<Record<BulkField, string>> = {};
  for (const f of BULK_FIELDS) {
    const v = values[f]?.trim();
    if (v) out[f] = v;
  }
  return out;
}

/** Something about a book worth an admin's attention, as a tile's flag. */
export type TileFlag = 'cover' | 'match' | 'transcode' | 'edited';

/** Whether a book carries an identifier the community metadata can match on. */
export const isMatched = (b: Pick<AdminBook, 'asin' | 'isbn'>) => !!(b.asin || b.isbn);

/**
 * A tile's flags, most actionable first, at most two: no cover, not matched
 * (only while community metadata is on), transcodes, edited.
 */
export function tileFlags(b: AdminBook, metadataOn: boolean): TileFlag[] {
  const flags: TileFlag[] = [];
  if (!b.has_cover) flags.push('cover');
  if (metadataOn && !isMatched(b)) flags.push('match');
  if (!b.direct_playable) flags.push('transcode');
  if (b.edited) flags.push('edited');
  return flags.slice(0, 2);
}

/** A "Continue curating" entry: the book and the first thing it needs. */
export interface CurateItem {
  book: AdminBook;
  issue: 'cover' | 'match';
}

/**
 * The "Continue curating" shelf: books missing a cover, then unmatched ones,
 * each book once (under its first issue), at most `max`.
 */
export function curatingShelf(
  noCover: AdminBook[],
  unmatched: AdminBook[],
  max = SHELF_SIZE,
): CurateItem[] {
  const seen = new Set<string>();
  const out: CurateItem[] = [];
  const add = (books: AdminBook[], issue: CurateItem['issue']) => {
    for (const book of books) {
      const k = selectionKey(book);
      if (out.length >= max || seen.has(k)) continue;
      seen.add(k);
      out.push({ book, issue });
    }
  };
  add(noCover, 'cover');
  add(unmatched, 'match');
  return out;
}

/** The cover grid's geometry: matches .shelf-grid in globals.css. */
export interface GridLayout {
  columns: number;
  columnGap: number;
  rowGap: number;
  /** One row of tiles: the square cover plus the title and subtitle under it. */
  rowHeight: number;
}

const MIN_COLUMN = 158;
/** The tile text under the cover: a 10px gap, two title lines and a subtitle. */
const TILE_META = 66;
/** The width assumed before the grid has been measured (and in jsdom, which has no layout). */
const FALLBACK_WIDTH = { desktop: 1024, phone: 360 };

/** Columns and row height for a grid `width` wide (phones always get two columns). */
export function gridLayout(width: number, phone: boolean): GridLayout {
  const w = width > 0 ? width : phone ? FALLBACK_WIDTH.phone : FALLBACK_WIDTH.desktop;
  const columnGap = phone ? 14 : 22;
  const rowGap = phone ? 22 : 28;
  const columns = phone ? 2 : Math.max(1, Math.floor((w + columnGap) / (MIN_COLUMN + columnGap)));
  const column = (w - columnGap * (columns - 1)) / columns;
  return { columns, columnGap, rowGap, rowHeight: Math.round(column + TILE_META) };
}

/** Books cut into grid rows of `columns`. */
export function toRows<T>(items: T[], columns: number): T[][] {
  const rows: T[][] = [];
  for (let i = 0; i < items.length; i += columns) rows.push(items.slice(i, i + columns));
  return rows;
}

/**
 * Whether the list should fetch its next page: the last rendered row is within
 * `ahead` rows of the end, there is a next page, and none is loading.
 */
export function shouldLoadMore(
  lastIndex: number | undefined,
  count: number,
  hasMore: boolean,
  loading: boolean,
  ahead = 2,
): boolean {
  return hasMore && !loading && lastIndex !== undefined && lastIndex >= count - 1 - ahead;
}

/** A length as "27h 18m" ("45m" under an hour), in the language's units. "" when unknown. */
export function formatDuration(seconds: number, lang: string): string {
  if (!Number.isFinite(seconds) || seconds <= 0) return '';
  const total = Math.round(seconds / 60);
  const h = Math.floor(total / 60);
  const m = total % 60;
  const unit = (n: number, u: 'hour' | 'minute') =>
    new Intl.NumberFormat(lang, { style: 'unit', unit: u, unitDisplay: 'narrow' }).format(n);
  if (!h) return unit(m, 'minute');
  return m ? `${unit(h, 'hour')} ${unit(m, 'minute')}` : unit(h, 'hour');
}

/** A format as a chip shows it: "m4b" reads "M4B". */
export const formatLabel = (format: string) => format.toUpperCase();
