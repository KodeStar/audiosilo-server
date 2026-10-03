import { useEffect, useRef } from 'react';
import { useQuery, useQueryClient, type QueryClient } from '@tanstack/react-query';
import { api, fetchCover } from './client';
import type { AdminLibrary } from './types';

// Query keys live here so invalidation and the hooks can't drift apart.
export const keys = {
  server: ['server'] as const,
  stats: ['admin', 'stats'] as const,
  settings: ['admin', 'settings'] as const,
  cover: (libraryId: number, path: string) => ['cover', libraryId, path] as const,
  libraries: ['admin', 'libraries'] as const,
  recentBooks: (libraryId: number) => ['books', 'recent', libraryId] as const,
  browse: (libraryId: number, path: string) => ['fs', libraryId, path] as const,
  dirs: (path: string) => ['admin', 'dirs', path] as const,
  users: ['admin', 'users'] as const,
  user: (id: number) => ['admin', 'user', id] as const,
  invites: ['admin', 'invites'] as const,
  shares: ['admin', 'shares'] as const,
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

/** A cover as a data: URL (null = the book has no art). Covers rarely change: cache for an hour. */
export function useCover(libraryId: number, path: string) {
  return useQuery({
    queryKey: keys.cover(libraryId, path),
    queryFn: () => fetchCover(libraryId, path),
    staleTime: 60 * 60_000,
    gcTime: 60 * 60_000,
    retry: false,
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

export function useUsers() {
  return useQuery({ queryKey: keys.users, queryFn: () => api.users().then((r) => r.users ?? []) });
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

export function useShares() {
  return useQuery({
    queryKey: keys.shares,
    queryFn: () => api.shares().then((r) => r.shares ?? []),
  });
}
