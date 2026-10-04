import { useEffect, useRef } from 'react';
import {
  keepPreviousData,
  useInfiniteQuery,
  useQuery,
  useQueryClient,
  type InfiniteData,
  type QueryClient,
} from '@tanstack/react-query';
import { refKey } from '@/lib/book-route';
import { compact } from '@/lib/utils';
import { api, type BookFilter, type BookListParams, type MatchBy, type ThumbSize } from './client';
import { loadThumb } from './cover-batch';
import type {
  AdminBook,
  AdminBookDetail,
  AdminBookPage,
  AdminLibrary,
  BookRef,
  PersonField,
} from './types';

// Query keys live here so invalidation and the hooks can't drift apart.
export const keys = {
  server: ['server'] as const,
  stats: ['admin', 'stats'] as const,
  settings: ['admin', 'settings'] as const,
  thumb: (libraryId: number, path: string, size: ThumbSize) =>
    ['thumb', libraryId, path, size] as const,
  libraries: ['admin', 'libraries'] as const,
  recentBooks: (libraryId: number) => ['books', 'recent', libraryId] as const,
  /** Every listing of one library's folders (a prefix: a scan changes them). */
  browseLibrary: (libraryId: number) => ['fs', libraryId] as const,
  browse: (libraryId: number, path: string) => ['fs', libraryId, path] as const,
  dirs: (path: string) => ['admin', 'dirs', path] as const,
  users: ['admin', 'users'] as const,
  user: (id: number) => ['admin', 'user', id] as const,
  invites: ['admin', 'invites'] as const,
  shares: ['admin', 'shares'] as const,
  /** Every admin book list and facet count (a prefix: invalidate after any edit). */
  books: ['admin', 'books'] as const,
  /** Every loaded book list (a prefix). */
  bookLists: ['admin', 'books', 'list'] as const,
  bookList: (params: BookListParams) => ['admin', 'books', 'list', params] as const,
  bookFacets: (filter: BookFilter) => ['admin', 'books', 'facets', filter] as const,
  people: (field: PersonField, libraryId?: number) =>
    ['admin', 'books', 'people', field, libraryId ?? 0] as const,
  series: (libraryId?: number) => ['admin', 'books', 'series', libraryId ?? 0] as const,
  book: (libraryId: number, path: string) => ['admin', 'book', libraryId, path] as const,
  match: (libraryId: number, path: string, by: MatchBy) =>
    ['admin', 'book', libraryId, path, 'match', by] as const,
  bookMeta: (libraryId: number, path: string) => ['meta', libraryId, path] as const,
};

/**
 * Refetches everything a change to accounts, access or invites can touch: the
 * people list, every open user page, invites, shares (members) and the overview.
 */
export function invalidatePeople(qc: QueryClient) {
  for (const key of [keys.users, ['admin', 'user'], keys.invites, keys.shares, keys.stats]) {
    void qc.invalidateQueries({ queryKey: key });
  }
}

/** Refetches what a library change touches: the list, the overview, shares (labels). */
export function invalidateLibraries(qc: QueryClient) {
  for (const key of [keys.libraries, keys.stats, keys.shares]) {
    void qc.invalidateQueries({ queryKey: key });
  }
}

export function useServerInfo() {
  return useQuery({ queryKey: keys.server, queryFn: api.serverInfo, staleTime: 5 * 60_000 });
}

/** Catalog totals + who's listening. Polled so "listening now" stays live. */
export function useStats() {
  return useQuery({ queryKey: keys.stats, queryFn: api.stats, refetchInterval: 30_000 });
}

export function useSettings() {
  return useQuery({ queryKey: keys.settings, queryFn: api.settings, staleTime: 5 * 60_000 });
}

/**
 * A cover thumbnail as a data: URL (null = the book has no art), batched with
 * every other cover asked for in the same moment (cover-batch.ts). Covers rarely
 * change: fresh for an hour (invalidateCover after an upload), but dropped five
 * minutes after the last cover using it unmounts, so scrolling a long grid
 * doesn't hold every data: URL in memory.
 */
export function useCover(libraryId: number, path: string, size: ThumbSize = 320) {
  return useQuery({
    queryKey: keys.thumb(libraryId, path, size),
    queryFn: ({ signal }) => loadThumb({ library_id: libraryId, path }, size, signal),
    staleTime: 60 * 60_000,
    gcTime: 5 * 60_000,
    retry: false,
  });
}

/** Refetches a book's cover everywhere it shows (every thumbnail size). */
export function invalidateCover(qc: QueryClient, libraryId: number, path: string) {
  void qc.invalidateQueries({ queryKey: ['thumb', libraryId, path] });
}

/** Whether a query key is one of these books' pages (or a match search on one). */
function bookPageOf(refs: BookRef[]) {
  const wanted = new Set(refs.map(refKey));
  return ([scope, kind, libraryId, path]: readonly unknown[]) =>
    scope === 'admin' &&
    kind === 'book' &&
    wanted.has(refKey({ library_id: libraryId as number, path: path as string }));
}

/** Refetches the pages of these books (what shares include them, say). */
export function invalidateBookPages(qc: QueryClient, refs: BookRef[]) {
  const isPage = bookPageOf(refs);
  void qc.invalidateQueries({ predicate: (q) => isPage(q.queryKey) });
}

/**
 * Refetches what a metadata edit touches: every book list, facet count and
 * aggregate, the edited books' pages, and the overview (titles in "listening").
 */
export function invalidateBooks(qc: QueryClient, edited: BookRef[] = []) {
  const isPage = bookPageOf(edited);
  void qc.invalidateQueries({
    predicate: ({ queryKey }) => {
      const [scope, kind] = queryKey;
      return (scope === 'admin' && (kind === 'books' || kind === 'stats')) || isPage(queryKey);
    },
  });
}

/**
 * Writes an edit's answer (the updated book page) into the cache: the book page,
 * and the edited row in place in every loaded list (so a long scrolled list
 * doesn't refetch page by page), then refetches what counts books (facets, the
 * people and series aggregates, the overview).
 */
export function settleBookEdit(qc: QueryClient, detail: AdminBookDetail) {
  const book = detail.book;
  qc.setQueryData(keys.book(book.library_id, book.path), detail);
  const key = refKey(book);
  const patch = (page: AdminBookPage): AdminBookPage =>
    page.books?.some((b) => refKey(b) === key)
      ? { ...page, books: page.books.map((b): AdminBook => (refKey(b) === key ? book : b)) }
      : page;
  qc.setQueriesData<AdminBookPage | InfiniteData<AdminBookPage>>(
    { queryKey: keys.bookLists },
    (data) =>
      data && 'pages' in data ? { ...data, pages: data.pages.map(patch) } : data && patch(data),
  );
  void qc.invalidateQueries({
    predicate: ({ queryKey: [scope, kind, sub] }) =>
      scope === 'admin' && (kind === 'stats' || (kind === 'books' && sub !== 'list')),
  });
}

/**
 * Libraries with book counts, root availability and scan progress. Polled every
 * second while any library scans (wherever the scan was started: this page, the
 * palette, another admin) and every minute otherwise, so a share that drops or
 * comes back shows up without a reload.
 */
export function useLibraries() {
  return useQuery({
    queryKey: keys.libraries,
    queryFn: () => api.libraries().then((r) => r.libraries),
    staleTime: 30_000,
    refetchInterval: (q) => (q.state.data?.some((l) => l.scan.running) ? 1000 : 60_000),
  });
}

/** The libraries whose folder can't be read right now. */
export function useOfflineLibraries() {
  return (useLibraries().data ?? []).filter((l) => !l.available);
}

/** Scans this console started, by library id, with when (see noteScanStarted). */
const startedScans = new Map<number, number>();

/** How long a started scan is still reported as finished (after that it's old news). */
const STARTED_SCAN_TTL_MS = 2 * 60_000;

/**
 * Records that this console started a scan of a library, then refetches the
 * list. The server reports the scan running before answering, but a small
 * library can finish before the list is fetched again, so the scan watcher
 * reports a started scan even if no poll ever saw it running.
 */
export function noteScanStarted(qc: QueryClient, libraryId: number) {
  startedScans.set(libraryId, Date.now());
  // A poll already in flight carries the state from before the scan: drop it.
  void qc.cancelQueries({ queryKey: keys.libraries });
  void qc.invalidateQueries({ queryKey: keys.libraries });
}

type ScanListener = (library: AdminLibrary) => void;
const scanListeners = new Set<ScanListener>();

/**
 * Watches every library's scan, mounted once (the shell): when one ends (seen
 * running, or started here, and now not running) it refetches what a scan
 * changes (the overview's counts, that library's newest books and folder
 * listings, every admin book list and aggregate), then tells the screens that
 * asked (useScanFinished).
 */
export function useScanWatcher() {
  const qc = useQueryClient();
  // dataUpdatedAt, not just data: a refetch that changed nothing keeps the same
  // data object, and a scan started here may have been over by then.
  const { data: libraries, dataUpdatedAt } = useLibraries();
  const running = useRef(new Set<number>());
  useEffect(() => {
    if (!libraries) return;
    for (const l of libraries) {
      if (l.scan.running) continue;
      const started = startedScans.get(l.id);
      const startedHere = started !== undefined && Date.now() - started < STARTED_SCAN_TTL_MS;
      if (started !== undefined) startedScans.delete(l.id);
      if (!running.current.has(l.id) && !startedHere) continue;
      for (const key of [
        keys.stats,
        keys.recentBooks(l.id),
        keys.books,
        keys.browseLibrary(l.id),
      ]) {
        void qc.invalidateQueries({ queryKey: key });
      }
      for (const fn of scanListeners) fn(l);
    }
    running.current = new Set(libraries.filter((l) => l.scan.running).map((l) => l.id));
  }, [libraries, dataUpdatedAt, qc]);
}

/** Calls `onFinish` when a library's scan ends, while mounted (the scan watcher has refetched by then). */
export function useScanFinished(onFinish: ScanListener) {
  const latest = useRef(onFinish);
  useEffect(() => {
    latest.current = onFinish;
  });
  useEffect(() => {
    const fn: ScanListener = (l) => latest.current(l);
    scanListeners.add(fn);
    return () => {
      scanListeners.delete(fn);
    };
  }, []);
}

export function useRecentBooks(libraryId: number, limit: number) {
  return useQuery({
    queryKey: keys.recentBooks(libraryId),
    queryFn: () => api.recentBooks(libraryId, limit).then((r) => r.books ?? []),
    staleTime: 5 * 60_000,
  });
}

/** One folder of a library (the share and detection pickers). */
export function useBrowse(libraryId: number | undefined, path: string) {
  return useQuery({
    queryKey: keys.browse(libraryId ?? 0, path),
    queryFn: () => api.browse(libraryId!, path),
    enabled: libraryId !== undefined,
  });
}

/** One folder of the server's filesystem (the library-root picker). */
export function useDirs(path: string) {
  // No retries: a folder that didn't answer (a dead network mount) won't the
  // second time either, and each retry would leave another stuck read behind.
  return useQuery({ queryKey: keys.dirs(path), queryFn: () => api.dirs(path), retry: false });
}

/** How long the palette's aggregates stay fresh: it reopens often, and a search needn't refetch them. */
export const PALETTE_STALE_MS = 5 * 60_000;

export function useUsers(enabled = true, staleTime?: number) {
  return useQuery({
    queryKey: keys.users,
    queryFn: () => api.users().then((r) => r.users ?? []),
    enabled,
    staleTime,
  });
}

export function useUser(id: number) {
  return useQuery({ queryKey: keys.user(id), queryFn: () => api.user(id) });
}

export function useInvites() {
  return useQuery({
    queryKey: keys.invites,
    queryFn: () => api.invites().then((r) => r.invites ?? []),
  });
}

export function useShares(enabled = true, staleTime?: number) {
  return useQuery({
    queryKey: keys.shares,
    queryFn: () => api.shares().then((r) => r.shares ?? []),
    enabled,
    staleTime,
  });
}

/**
 * The admin book list, a keyset page at a time (`fetchNextPage` for more). The
 * previous result stays on screen while a new filter loads, so the grid doesn't
 * flash empty on every keystroke.
 */
export function useAdminBooks(params: Omit<BookListParams, 'cursor'>, enabled = true) {
  return useInfiniteQuery({
    queryKey: keys.bookList(params),
    queryFn: ({ pageParam }) => api.adminBooks({ ...params, cursor: pageParam }),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (last) => last.next_cursor || undefined,
    placeholderData: keepPreviousData,
    enabled,
  });
}

/** One page of the admin book list (a shelf, a palette search): no paging. */
export function useAdminBookPage(params: BookListParams, enabled = true) {
  return useQuery({
    queryKey: keys.bookList(params),
    queryFn: () => api.adminBooks(params),
    enabled,
  });
}

export function useBookFacets(filter: BookFilter) {
  return useQuery({
    queryKey: keys.bookFacets(filter),
    queryFn: () => api.bookFacets(filter),
    placeholderData: keepPreviousData,
  });
}

/** The authors or narrators of one library (or all), with merge suggestions. */
export function usePeople(
  field: PersonField,
  libraryId?: number,
  enabled = true,
  staleTime?: number,
) {
  return useQuery({
    queryKey: keys.people(field, libraryId),
    queryFn: () => api.people(field, libraryId),
    enabled,
    staleTime,
  });
}

export function useSeries(libraryId?: number, enabled = true, staleTime?: number) {
  return useQuery({
    queryKey: keys.series(libraryId),
    queryFn: () => api.series(libraryId).then((r) => r.series ?? []),
    enabled,
    staleTime,
  });
}

/** The book page: fields with provenance, chapters, files, listeners, shares. */
export function useAdminBook(libraryId: number, path: string) {
  return useQuery({
    queryKey: keys.book(libraryId, path),
    queryFn: () => api.adminBook(libraryId, path),
  });
}

/**
 * Community works a book might be. Searched only when asked (the match dialog is
 * open); a failed search is not retried (metaserve down answers 502).
 */
export function useMatchCandidates(libraryId: number, path: string, by: MatchBy, enabled: boolean) {
  // The key and the request share one cleaning, so equal searches share a cache entry.
  const clean = compact(by);
  return useQuery({
    queryKey: keys.match(libraryId, path, clean),
    queryFn: () => api.matchBook(libraryId, path, clean).then((r) => r.candidates ?? []),
    enabled,
    retry: false,
    staleTime: 5 * 60_000,
  });
}

/**
 * A book's community metadata (its series rails). The server caches it for a day;
 * here it is kept for the session.
 */
export function useBookMeta(libraryId: number, path: string, enabled: boolean) {
  return useQuery({
    queryKey: keys.bookMeta(libraryId, path),
    queryFn: () => api.bookMeta(libraryId, path),
    enabled,
    retry: false,
    staleTime: 60 * 60_000,
  });
}
