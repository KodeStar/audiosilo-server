import { useQuery } from '@tanstack/react-query';
import { api, fetchCover } from './client';

// Query keys live here so invalidation and the hooks can't drift apart.
export const keys = {
  server: ['server'] as const,
  stats: ['admin', 'stats'] as const,
  settings: ['admin', 'settings'] as const,
  cover: (libraryId: number, path: string) => ['cover', libraryId, path] as const,
};

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
