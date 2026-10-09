import { useEffect, useRef } from 'react';
import {
  keepPreviousData,
  queryOptions,
  useInfiniteQuery,
  useQueries,
  useQuery,
  useQueryClient,
  type InfiniteData,
  type QueryClient,
  type UseQueryResult,
} from '@tanstack/react-query';
import { refKey, refOf } from '@/lib/book-route';
import { compact } from '@/lib/utils';
import { api, type BookFilter, type BookListParams, type MatchBy, type ThumbSize } from './client';
import { loadCommunityCover, loadThumb } from './cover-batch';
import { loadWork, type WorkAnswer } from './work-batch';
import type {
  AdminBook,
  AdminBookDetail,
  AdminBookPage,
  AdminLibrary,
  AuditFilter,
  BookRef,
  Import,
  FolderMode,
  MatchOutcome,
  MatchRun,
  MetaMirrorStatus,
  PersonField,
  ServerEventKind,
  SessionCursor,
  SessionFilter,
  SystemStatus,
} from './types';

// Query keys live here so invalidation and the hooks can't drift apart.
export const keys = {
  server: ['server'] as const,
  stats: ['admin', 'stats'] as const,
  settings: ['admin', 'settings'] as const,
  system: ['admin', 'system'] as const,
  metaMirror: ['admin', 'meta-mirror'] as const,
  update: ['admin', 'update'] as const,
  logs: (level: string, q: string) => ['admin', 'logs', level, q] as const,
  backups: ['admin', 'backups'] as const,
  notifyTargets: ['admin', 'notifications'] as const,
  events: ['admin', 'events'] as const,
  eventList: (kind: ServerEventKind | undefined) =>
    ['admin', 'events', 'list', kind ?? ''] as const,
  audit: (filter: AuditFilter) => ['admin', 'audit', filter] as const,
  /** Every cover thumbnail (a prefix). */
  thumbs: ['thumb'] as const,
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
  /** Every book page of every library (a prefix). */
  allBookPages: ['admin', 'book'] as const,
  /** Every book page of one library (a prefix: a scan changes them). */
  bookPages: (libraryId: number) => ['admin', 'book', libraryId] as const,
  book: (libraryId: number, path: string) => ['admin', 'book', libraryId, path] as const,
  match: (libraryId: number, path: string, by: MatchBy) =>
    ['admin', 'book', libraryId, path, 'match', by] as const,
  communityCover: (url: string, size: ThumbSize) => ['admin', 'communityCover', size, url] as const,
  bookMeta: (libraryId: number, path: string) => ['meta', libraryId, path] as const,
  /** A book's community work, by identity and identifiers: a new ASIN or ISBN asks again. */
  bookWork: (b: AdminBook) => ['meta', 'work', b.library_id, b.path, b.asin, b.isbn] as const,
  /** The Health summary and the duplicate groups (a prefix: a scan or an ignore changes them). */
  issues: ['admin', 'issues'] as const,
  issueSummary: ['admin', 'issues', 'summary'] as const,
  duplicates: (ignored: boolean) => ['admin', 'issues', 'duplicates', ignored] as const,
  jobs: ['admin', 'jobs'] as const,
  /** Bulk match runs and every page of their items (a prefix). */
  matchRuns: ['admin', 'match-runs'] as const,
  matchRunItems: (id: number, outcome: MatchOutcome) =>
    ['admin', 'match-runs', id, 'items', outcome] as const,
  /** Every page of scan history (a prefix). */
  scanRuns: ['admin', 'scan-runs'] as const,
  scanRunList: (libraryId: number) => ['admin', 'scan-runs', 'list', libraryId] as const,
  scanRun: (id: number) => ['admin', 'scan-runs', id] as const,
  /** Every Activity period and listening-days query (a prefix). */
  activityAll: ['admin', 'activity'] as const,
  activity: (range: string) => ['admin', 'activity', range] as const,
  listeningDays: (range: string, userId: number) =>
    ['admin', 'activity', 'days', range, userId] as const,
  /** Live sessions and every session list (a prefix). */
  sessions: ['admin', 'sessions'] as const,
  liveSessions: ['admin', 'sessions', 'live'] as const,
  sessionList: (filter: SessionFilter, limit: number) =>
    ['admin', 'sessions', 'list', filter, limit] as const,
  /** Everyone's devices and each person's (a prefix). */
  devices: ['admin', 'devices'] as const,
  userDevices: (userId: number) => ['admin', 'devices', userId] as const,
  userProgress: (userId: number) => ['admin', 'user', userId, 'progress'] as const,
  /** Every import list: everyone's (0) and each person's (a prefix). */
  importLists: ['admin', 'imports', 'list'] as const,
  importList: (userId: number) => ['admin', 'imports', 'list', userId] as const,
  importDetail: (id: number) => ['admin', 'imports', 'detail', id] as const,
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
 * What the server depends on (Health > System; Settings and About read parts).
 * Health > System polls it every 30 seconds (`poll`); elsewhere it's fresh for a minute.
 */
export function useSystem({ poll = false } = {}) {
  return useQuery({
    queryKey: keys.system,
    queryFn: api.system,
    staleTime: 60_000,
    refetchInterval: poll ? 30_000 : false,
  });
}

/**
 * The local metadata copy is busy: downloading, its check running (no next check
 * yet), or a check due now (Check now was pressed). Its progress is worth following.
 */
export function mirrorBusy(m: MetaMirrorStatus | undefined, now: number = Date.now()): boolean {
  if (!m) return false;
  return m.state === 'downloading' || !m.next_check_at || Date.parse(m.next_check_at) <= now;
}

/** Puts the local copy's status into the cached system status. */
function mergeMirror(qc: QueryClient, mirror: MetaMirrorStatus) {
  qc.setQueryData<SystemStatus>(keys.system, (s) =>
    s ? { ...s, metadata: { ...s.metadata, mirror } } : s,
  );
}

/**
 * Follows the local metadata copy while it is busy (Health > System): polls only its
 * status (GET /admin/meta/mirror) every 2 seconds, merges each answer into the system
 * status, and fetches the whole system status once when the copy is no longer busy.
 * `mirror` is the copy in use (activeMirror), undefined when there is none.
 */
export function useMirrorPoll(mirror: MetaMirrorStatus | undefined) {
  const qc = useQueryClient();
  useQuery({
    queryKey: keys.metaMirror,
    queryFn: async () => {
      const m = await api.metaMirror();
      mergeMirror(qc, m);
      if (!mirrorBusy(m)) void qc.invalidateQueries({ queryKey: keys.system });
      return m;
    },
    enabled: mirrorBusy(mirror),
    refetchInterval: 2000,
  });
}

/**
 * Asks the server to check for a newer copy of the metadata now. Its answer (the next
 * check due now) goes into the system status, which makes useMirrorPoll follow the
 * check. Throws the ApiError (metadata_off, not_mirror_mode) for the caller to show.
 */
export async function checkMetaMirror(qc: QueryClient): Promise<MetaMirrorStatus> {
  const mirror = await api.checkMetaMirror();
  mergeMirror(qc, mirror);
  return mirror;
}

/** The backups, their state and any restore waiting; polled each second while one is made. */
export function useBackups() {
  return useQuery({
    queryKey: keys.backups,
    queryFn: api.backups,
    refetchInterval: (q) => (q.state.data?.status.running ? 1000 : false),
  });
}

/** The notification destinations, with the event and destination kinds the server knows. */
export function useNotifyTargets() {
  return useQuery({ queryKey: keys.notifyTargets, queryFn: api.notifyTargets });
}

/** The bell's feed: the newest events, checked each minute. */
export function useServerEvents() {
  return useQuery({
    queryKey: keys.events,
    queryFn: () => api.serverEvents({ limit: 20 }),
    staleTime: 30_000,
    refetchInterval: 60_000,
  });
}

/**
 * Server > Events: the whole feed (90 days), newest first, a page at a time,
 * maybe one kind. No polling (it would refetch every page loaded); it refreshes
 * on focus, and the bell keeps checking for new events.
 */
export function useServerEventList(kind: ServerEventKind | undefined) {
  return useInfiniteQuery({
    queryKey: keys.eventList(kind),
    queryFn: ({ pageParam }) => api.serverEvents({ kind, before: pageParam, limit: 50 }),
    initialPageParam: undefined as number | undefined,
    getNextPageParam: (last) => last.next_before || undefined,
    placeholderData: keepPreviousData,
  });
}

/** The audit log, newest first, a page at a time (`fetchNextPage` for older ones). */
export function useAudit(filter: AuditFilter) {
  return useInfiniteQuery({
    queryKey: keys.audit(filter),
    queryFn: ({ pageParam }) => api.audit({ ...filter, before: pageParam, limit: 50 }),
    initialPageParam: undefined as number | undefined,
    getNextPageParam: (last) => last.next_before || undefined,
    placeholderData: keepPreviousData,
  });
}

/** The update check's state (Overview's server card, About). */
export function useUpdateStatus() {
  return useQuery({ queryKey: keys.update, queryFn: api.updateStatus, staleTime: 10 * 60_000 });
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

/**
 * Refetches what a new or removed custom cover touches: the art, the book page and
 * the book lists and counts (has_cover, custom_cover, the no_cover issue).
 */
export function invalidateBookCover(qc: QueryClient, ref: BookRef) {
  invalidateCover(qc, ref.library_id, ref.path);
  void qc.invalidateQueries({ queryKey: keys.book(ref.library_id, ref.path), exact: true });
  invalidateBooks(qc);
}

/**
 * Refetches what reads a library's books differently after they were re-read or
 * re-resolved (a scan, a new metadata source): the lists, pages, counts and
 * browse views.
 */
export function invalidateLibraryBooks(qc: QueryClient, libraryId: number) {
  for (const key of [
    keys.stats,
    keys.recentBooks(libraryId),
    keys.books,
    keys.browseLibrary(libraryId),
    keys.bookPages(libraryId),
  ]) {
    void qc.invalidateQueries({ queryKey: key });
  }
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
const librariesQuery = {
  queryKey: keys.libraries,
  queryFn: () => api.libraries().then((r) => r.libraries),
};

export function useLibraries() {
  return useQuery({
    ...librariesQuery,
    staleTime: 30_000,
    refetchInterval: (q) => (q.state.data?.some(scanActive) ? 1000 : 60_000),
  });
}

/** Whether a library's scan runs or waits in the job queue. */
export function scanActive(l: AdminLibrary) {
  return l.scan.running || !!l.scan.queued;
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
  void qc.invalidateQueries({ queryKey: keys.jobs });
}

/**
 * Saves how a folder of a library reads (`null`: automatic), which rescans the
 * library: notes that scan (noteScanStarted) and refetches the library's folder
 * listings, so the override shows on every screen at once; the books it makes
 * follow when the rescan ends (the scan watcher).
 */
export async function setFolderMode(
  qc: QueryClient,
  libraryId: number,
  path: string,
  mode: FolderMode | null,
) {
  await api.setFolderOverride(libraryId, path, mode);
  noteScanStarted(qc, libraryId);
  await qc.invalidateQueries({ queryKey: keys.browseLibrary(libraryId) });
}

type ScanListener = (library: AdminLibrary) => void;
const scanListeners = new Set<ScanListener>();

/**
 * Watches every library's scan, mounted once (the shell): when one ends (seen
 * running or queued, or started here, and now neither) it refetches what a scan
 * changes (the overview's counts, that library's newest books, folder
 * listings and book pages, every admin book list and aggregate, the scan
 * history, and the Health issues once nothing scans), then tells the screens
 * that asked (useScanFinished).
 */
export function useScanWatcher() {
  const qc = useQueryClient();
  // dataUpdatedAt, not just data: a refetch that changed nothing keeps the same
  // data object, and a scan started here may have been over by then.
  const { data: libraries, dataUpdatedAt } = useLibraries();
  const running = useRef(new Set<number>());
  useEffect(() => {
    if (!libraries) return;
    let ended = false;
    for (const l of libraries) {
      if (scanActive(l)) continue;
      const started = startedScans.get(l.id);
      const startedHere = started !== undefined && Date.now() - started < STARTED_SCAN_TTL_MS;
      if (started !== undefined) startedScans.delete(l.id);
      if (!running.current.has(l.id) && !startedHere) continue;
      invalidateLibraryBooks(qc, l.id);
      void qc.invalidateQueries({ queryKey: keys.scanRuns });
      ended = true;
      for (const fn of scanListeners) fn(l);
    }
    running.current = new Set(libraries.filter(scanActive).map((l) => l.id));
    // The issues are a whole-index computation: refresh them once the queue is
    // idle, not after each scan of a "rescan every library".
    if (ended && running.current.size === 0) void qc.invalidateQueries({ queryKey: keys.issues });
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

/** The Health page's categories, offline libraries and last check. */
export function useIssues() {
  return useQuery({ queryKey: keys.issueSummary, queryFn: api.issues, staleTime: 30_000 });
}

/** Groups of copies of one book: the open ones, or with `ignored` the ignored ones. */
export function useDuplicates(ignored: boolean) {
  return useQuery({
    queryKey: keys.duplicates(ignored),
    queryFn: () => api.duplicates({ ignored: ignored || undefined }).then((r) => r.groups ?? []),
  });
}

/**
 * Refetches what an ignore, an un-ignore or a re-read changes: the Health summary
 * and duplicates, and the issue-filtered book lists (nothing else lists by issue).
 */
export function invalidateIssues(qc: QueryClient) {
  void qc.invalidateQueries({ queryKey: keys.issues });
  void qc.invalidateQueries({
    queryKey: keys.bookLists,
    predicate: ({ queryKey }) => !!(queryKey[3] as BookListParams | undefined)?.issue,
  });
}

/**
 * Refetches what writing or clearing community matches changes, across many books
 * at once: every book list and page, the Health issues ("Not matched"), and the
 * covers.
 */
export function invalidateMatches(qc: QueryClient) {
  invalidateBooks(qc);
  invalidateIssues(qc);
  void qc.invalidateQueries({ queryKey: keys.allBookPages });
  void qc.invalidateQueries({ queryKey: keys.thumbs });
}

/**
 * Reads a book's files again now (POST .../book/rescan) and writes the fresh page
 * everywhere it shows and its cover, then refetches the Health issues (a book the
 * re-read fixed leaves its issue lists) unless `refreshIssues` is false (a batch
 * refreshes once at the end). Returns the page.
 */
export async function rescanBook(
  qc: QueryClient,
  ref: BookRef,
  refreshIssues = true,
): Promise<AdminBookDetail> {
  const fresh = await api.rescanBook(ref.library_id, ref.path);
  settleBookEdit(qc, fresh);
  invalidateCover(qc, ref.library_id, ref.path);
  if (refreshIssues) invalidateIssues(qc);
  return fresh;
}

/**
 * Each library's root folder by id, stable while the roots don't change (the
 * library list itself changes every second during a scan).
 */
export function useLibraryRoots(): Record<number, string> {
  return (
    useQuery({
      ...librariesQuery,
      staleTime: 30_000,
      select: (libs) => Object.fromEntries(libs.map((l) => [l.id, l.root])),
    }).data ?? NO_ROOTS
  );
}
const NO_ROOTS: Record<number, string> = {};

/**
 * The running scan, the queue and the schedules: polled every second while
 * anything runs or waits, every 15 seconds otherwise.
 */
export function useJobs() {
  return useQuery({
    queryKey: keys.jobs,
    queryFn: api.jobs,
    refetchInterval: (q) =>
      q.state.data && (q.state.data.running || q.state.data.queued.length) ? 1000 : 15_000,
  });
}

/** Whether a bulk match run is still working. */
export function matchRunActive(r: MatchRun | undefined) {
  return r?.status === 'matching' || r?.status === 'applying';
}

/** Bulk match runs, newest first; polled each second while one works. */
export function useMatchRuns() {
  return useQuery({
    queryKey: keys.matchRuns,
    queryFn: api.matchRuns,
    refetchInterval: (q) => (q.state.data?.runs.some(matchRunActive) ? 1000 : false),
  });
}

/** One outcome's books of a match run, a page at a time (the review). */
export function useMatchRunItems(id: number, outcome: MatchOutcome) {
  return useInfiniteQuery({
    queryKey: keys.matchRunItems(id, outcome),
    queryFn: ({ pageParam }) => api.matchRunItems(id, { outcome, after: pageParam, limit: 50 }),
    initialPageParam: undefined as number | undefined,
    getNextPageParam: (last) => last.next_after || undefined,
  });
}

/** Scan history, newest first, a page at a time (`fetchNextPage` for older runs). */
export function useScanRuns(libraryId = 0) {
  return useInfiniteQuery({
    queryKey: keys.scanRunList(libraryId),
    queryFn: ({ pageParam }) =>
      api.scanRuns({ library_id: libraryId || undefined, before: pageParam, limit: 30 }),
    initialPageParam: undefined as number | undefined,
    getNextPageParam: (last) => last.next_before,
  });
}

/** One recorded scan with its log (mounted when its log is opened). */
export function useScanRun(id: number) {
  return useQuery({ queryKey: keys.scanRun(id), queryFn: () => api.scanRun(id) });
}

/** The book page: fields with provenance, chapters, files, listeners, shares. */
export function useAdminBook(libraryId: number, path: string) {
  return useQuery({
    queryKey: keys.book(libraryId, path),
    queryFn: () => api.adminBook(libraryId, path),
    // A community chapter check runs in the background: follow it to the end.
    refetchInterval: (q) => (q.state.data?.community_checking ? CHECK_POLL_MS : false),
  });
}

/** How often a book page asks again while its community chapters are being checked. */
const CHECK_POLL_MS = 3000;

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
 * A community cover image (a match candidate's cover_url) as a thumbnail data:
 * URL the server fetched (null = it couldn't), batched with every other cover
 * asked for in the same moment (cover-batch.ts). Keyed by URL, so the candidate
 * list and the compare step share one answer. Not retried: the dialog shows a
 * generated cover instead.
 */
export function useCommunityCover(url: string, size: ThumbSize = 160) {
  return useQuery({
    queryKey: keys.communityCover(url, size),
    queryFn: ({ signal }) => loadCommunityCover(url, size, signal),
    enabled: !!url,
    retry: false,
    staleTime: 60 * 60_000,
  });
}

/**
 * What useCommunityCover already holds for a URL: its thumbnail, null when the
 * server couldn't fetch it, undefined before it has answered.
 */
export function cachedCommunityCover(
  qc: QueryClient,
  url: string,
  size: ThumbSize = 160,
): string | null | undefined {
  return qc.getQueryData<string | null>(keys.communityCover(url, size));
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

/**
 * How soon a book whose work may have failed to resolve is asked about again:
 * the server holds a failed lookup this long anyway (meta errorTTL).
 */
export const WORKS_RETRY_MS = 2 * 60_000;

/**
 * One book's community work (POST /admin/books/works, batched with every book
 * asked about in the same moment: work-batch.ts). A final answer is kept for the
 * session, so a resolved book is never sent again; one that may have failed is
 * asked again no sooner than WORKS_RETRY_MS (a remount or a window focus).
 */
export const bookWorkQuery = (b: AdminBook) =>
  queryOptions({
    queryKey: keys.bookWork(b),
    // The book's refKey rides along so the card's combine can key the answers.
    queryFn: async ({ signal }) => ({ key: refKey(b), ...(await loadWork(refOf(b), signal)) }),
    staleTime: (q) => (q.state.data?.final === false ? WORKS_RETRY_MS : Infinity),
    gcTime: Infinity,
    retry: false,
  });

/** The resolved work ids by refKey, and whether any book is still being asked about. */
function combineWorks(results: UseQueryResult<WorkAnswer & { key: string }>[]) {
  const ids = new Map<string, string>();
  for (const r of results) {
    if (r.data?.id) ids.set(r.data.key, r.data.id);
  }
  return { ids, pending: results.some((r) => r.isPending) };
}

/**
 * Which community work each book is, as work ids by refKey: the Series cards
 * place owned books on a rail by them. Books that didn't resolve are absent.
 */
export function useBookWorks(books: AdminBook[]) {
  return useQueries({ queries: books.map(bookWorkQuery), combine: combineWorks });
}

/**
 * Who is listening now, polled (players save every few seconds while they play):
 * every 10 seconds where the live list is the point, less often where it's a badge.
 */
export function useLiveSessions(interval = 10_000) {
  return useQuery({
    queryKey: keys.liveSessions,
    queryFn: () => api.liveSessions().then((r) => r.sessions),
    refetchInterval: interval,
  });
}

/**
 * The Activity page for a period: computed by the server on request, so a period
 * already seen stays fresh for a minute. With `keepPrevious`, the previous period
 * stays on screen while the next loads (the period picker).
 */
export function useActivity(range: string, keepPrevious = false) {
  return useQuery({
    queryKey: keys.activity(range),
    queryFn: () => api.activity(range),
    staleTime: 60_000,
    placeholderData: keepPrevious ? keepPreviousData : undefined,
  });
}

/** Listening per day over a period, of everyone (no id) or one person: no other stats. */
export function useListeningDays(range: string, userId = 0, enabled = true) {
  return useQuery({
    queryKey: keys.listeningDays(range, userId),
    queryFn: () => api.listeningDays(range, userId || undefined),
    staleTime: 60_000,
    enabled,
  });
}

/** Sessions newest first, `limit` at a time (`fetchNextPage` for older ones). */
export function useSessions(filter: SessionFilter, limit = 50) {
  return useInfiniteQuery({
    queryKey: keys.sessionList(filter, limit),
    queryFn: ({ pageParam }) => api.sessions({ ...filter, ...pageParam, limit }),
    initialPageParam: undefined as SessionCursor | undefined,
    // The last session's start rides along (before_at), so the next page continues
    // in place even if that session is gone by then (an undone import, a roll-up).
    getNextPageParam: (last): SessionCursor | undefined =>
      last.next_before == null
        ? undefined
        : { before: last.next_before, before_at: last.sessions.at(-1)?.started_at },
  });
}

/** Signed-in devices and API keys: one person's, or everyone's (no id). */
export function useDevices(userId?: number) {
  return useQuery({
    queryKey: userId ? keys.userDevices(userId) : keys.devices,
    queryFn: () => api.devices(userId).then((r) => r.devices),
  });
}

/** Every book a person has progress on, with start and finish dates. */
export function useUserProgress(userId: number) {
  return useQuery({
    queryKey: keys.userProgress(userId),
    queryFn: () => api.userProgress(userId).then((r) => r.progress),
  });
}

/**
 * Refetches what a progress edit changes: the person's progress and page, the
 * book's page (its listeners), the overview's listening feed and the Activity
 * stats (finished counts, the funnel).
 */
export function invalidateProgress(qc: QueryClient, userId: number, ref: BookRef) {
  for (const key of [
    keys.userProgress(userId),
    keys.book(ref.library_id, ref.path),
    keys.stats,
    keys.activityAll,
  ]) {
    void qc.invalidateQueries({ queryKey: key });
  }
}

/** Refetches what signing a device out changes: the device lists and who is live. */
export function invalidateDevices(qc: QueryClient) {
  void qc.invalidateQueries({ queryKey: keys.devices });
  void qc.invalidateQueries({ queryKey: keys.sessions });
}

/** The import statuses with work under way: polled until they settle. */
export function importBusy(imp: Pick<Import, 'status'> | undefined): boolean {
  return imp?.status === 'fetching' || imp?.status === 'applying';
}

/** How often a list with an import being fetched or applied is asked about. */
export const IMPORT_POLL_MS = 1500;

/**
 * Every import, or one person's (`userId`), newest first. Polled while any of
 * them is being fetched or applied: one request however many there are.
 */
export function useImports(userId = 0) {
  return useQuery({
    queryKey: keys.importList(userId),
    queryFn: () => api.imports(userId || undefined).then((r) => r.imports ?? []),
    refetchInterval: (q) => (q.state.data?.some(importBusy) ? IMPORT_POLL_MS : false),
  });
}

/** One import with its unmatched books (asked for only while `enabled`). */
export function useImportDetail(id: number, enabled = true) {
  return useQuery({
    queryKey: keys.importDetail(id),
    queryFn: () => api.importDetail(id),
    enabled,
  });
}

/**
 * Shows an import's new state (an apply's or a cutoff change's answer) in every
 * list holding it at once, until the lists are refetched.
 */
export function setImportRow(qc: QueryClient, imp: Import) {
  qc.setQueriesData<Import[]>({ queryKey: keys.importLists }, (old) =>
    old?.map((o) =>
      o.id === imp.id
        ? {
            ...o,
            status: imp.status,
            cutoff: imp.cutoff,
            cutoff_utc_offset: imp.cutoff_utc_offset,
            applied_at: imp.applied_at,
            summary: imp.summary,
          }
        : o,
    ),
  );
}

/**
 * Refetches what applying or undoing an import changes: the import lists and
 * that import, the person's progress and page, every book page (listeners),
 * sessions, the overview and the Activity stats.
 */
export function invalidateImported(qc: QueryClient, imp: Pick<Import, 'id' | 'user_id'>) {
  for (const key of [
    keys.importLists,
    keys.importDetail(imp.id),
    keys.user(imp.user_id),
    keys.allBookPages,
    keys.sessions,
    keys.stats,
    keys.activityAll,
  ]) {
    void qc.invalidateQueries({ queryKey: key });
  }
}
