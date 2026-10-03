import { useEffect, useRef } from 'react';
import {
  keepPreviousData,
  useInfiniteQuery,
  useQuery,
  useQueryClient,
  type QueryClient,
} from '@tanstack/react-query';
import { api, fetchCover, type BookFilter, type BookListParams } from './client';
import { loadThumb, type ThumbSize } from './cover-batch';
import type { AdminBookDetail, AdminLibrary, BookRef } from './types';

// Query keys live here so invalidation and the hooks can't drift apart.
export const keys = {
  server: ['server'] as const,
  stats: ['admin', 'stats'] as const,
  settings: ['admin', 'settings'] as const,
  cover: (libraryId: number, path: string) => ['cover', libraryId, path] as const,
  thumb: (libraryId: number, path: string, size: ThumbSize) =>
    ['thumb', libraryId, path, size] as const,
  libraries: ['admin', 'libraries'] as const,
  recentBooks: (libraryId: number) => ['books', 'recent', libraryId] as const,
  browse: (libraryId: number, path: string) => ['fs', libraryId, path] as const,
  dirs: (path: string) => ['admin', 'dirs', path] as const,
  users: ['admin', 'users'] as const,
  user: (id: number) => ['admin', 'user', id] as const,
  invites: ['admin', 'invites'] as const,
  shares: ['admin', 'shares'] as const,
  /** Every admin book list and facet count (a prefix: invalidate after any edit). */
  books: ['admin', 'books'] as const,
  bookList: (params: BookListParams) => ['admin', 'books', 'list', params] as const,
  bookFacets: (filter: BookFilter) => ['admin', 'books', 'facets', filter] as const,
  authors: (libraryId?: number) => ['admin', 'books', 'authors', libraryId ?? 0] as const,
  narrators: (libraryId?: number) => ['admin', 'books', 'narrators', libraryId ?? 0] as const,
  series: (libraryId?: number) => ['admin', 'books', 'series', libraryId ?? 0] as const,
  book: (libraryId: number, path: string) => ['admin', 'book', libraryId, path] as const,
  match: (libraryId: number, path: string, by: Record<string, string>) =>
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
 * A cover as a data: URL (null = the book has no art). `size` asks for a batched
 * thumbnail (grids, shelves, rows); `'full'` fetches the art itself (the book
 * hero). Covers rarely change: cache for an hour, and invalidateCover after an
 * upload.
 */
export function useCover(libraryId: number, path: string, size: ThumbSize | 'full' = 320) {
  return useQuery({
    queryKey: size === 'full' ? keys.cover(libraryId, path) : keys.thumb(libraryId, path, size),
    queryFn: () =>
      size === 'full'
        ? fetchCover(libraryId, path)
        : loadThumb({ library_id: libraryId, path }, size),
    staleTime: 60 * 60_000,
    gcTime: 60 * 60_000,
    retry: false,
  });
}

/** Refetches a book's cover everywhere it shows (full art and every thumbnail size). */
export function invalidateCover(qc: QueryClient, libraryId: number, path: string) {
  void qc.invalidateQueries({ queryKey: keys.cover(libraryId, path) });
  void qc.invalidateQueries({ queryKey: ['thumb', libraryId, path] });
}

/**
 * Refetches what a metadata edit touches: every book list, facet count and
 * aggregate, the edited books' pages, and the overview (titles in "listening").
 */
export function invalidateBooks(qc: QueryClient, edited: BookRef[] = []) {
  void qc.invalidateQueries({ queryKey: keys.books });
  void qc.invalidateQueries({ queryKey: keys.stats });
  for (const b of edited) void qc.invalidateQueries({ queryKey: keys.book(b.library_id, b.path) });
}

/** Writes an edit's answer (the updated book page) into the cache, then refetches the lists. */
export function settleBookEdit(qc: QueryClient, detail: AdminBookDetail) {
  qc.setQueryData(keys.book(detail.book.library_id, detail.book.path), detail);
  invalidateBooks(qc);
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
 * library can finish before the list is fetched again, so useScanFinished
 * reports a started scan even if no poll ever saw it running.
 */
export function noteScanStarted(qc: QueryClient, libraryId: number) {
  startedScans.set(libraryId, Date.now());
  // A poll already in flight carries the state from before the scan: drop it.
  void qc.cancelQueries({ queryKey: keys.libraries });
  void qc.invalidateQueries({ queryKey: keys.libraries });
}

/**
 * Calls `onFinish` when a library's scan ends (seen running, or started here,
 * and now not running), after refetching what a scan changes: the overview's
 * counts and that library's newest books.
 */
export function useScanFinished(onFinish: (library: AdminLibrary) => void) {
  const qc = useQueryClient();
  // dataUpdatedAt, not just data: a refetch that changed nothing keeps the same
  // data object, and a scan started here may have been over by then.
  const { data: libraries, dataUpdatedAt } = useLibraries();
  const finished = useRef(onFinish);
  useEffect(() => {
    finished.current = onFinish;
  });
  const running = useRef(new Set<number>());
  useEffect(() => {
    if (!libraries) return;
    for (const l of libraries) {
      const started = startedScans.get(l.id);
      const startedHere = started !== undefined && Date.now() - started < STARTED_SCAN_TTL_MS;
      if (l.scan.running) continue;
      if (started !== undefined) startedScans.delete(l.id);
      if (running.current.has(l.id) || startedHere) {
        void qc.invalidateQueries({ queryKey: keys.stats });
        void qc.invalidateQueries({ queryKey: keys.recentBooks(l.id) });
        finished.current(l);
      }
    }
    running.current = new Set(libraries.filter((l) => l.scan.running).map((l) => l.id));
  }, [libraries, dataUpdatedAt, qc]);
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

export function useUsers(enabled = true) {
  return useQuery({
    queryKey: keys.users,
    queryFn: () => api.users().then((r) => r.users ?? []),
    enabled,
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

export function useShares(enabled = true) {
  return useQuery({
    queryKey: keys.shares,
    queryFn: () => api.shares().then((r) => r.shares ?? []),
    enabled,
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

export function useAuthors(libraryId?: number, enabled = true) {
  return useQuery({
    queryKey: keys.authors(libraryId),
    queryFn: () => api.authors(libraryId),
    enabled,
  });
}

export function useNarrators(libraryId?: number, enabled = true) {
  return useQuery({
    queryKey: keys.narrators(libraryId),
    queryFn: () => api.narrators(libraryId),
    enabled,
  });
}

export function useSeries(libraryId?: number, enabled = true) {
  return useQuery({
    queryKey: keys.series(libraryId),
    queryFn: () => api.series(libraryId).then((r) => r.series ?? []),
    enabled,
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
export function useMatchCandidates(
  libraryId: number,
  path: string,
  by: { q?: string; asin?: string; isbn?: string },
  enabled: boolean,
) {
  const clean = Object.fromEntries(Object.entries(by).filter(([, v]) => v)) as Record<
    string,
    string
  >;
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
