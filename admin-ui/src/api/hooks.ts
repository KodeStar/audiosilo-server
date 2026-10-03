import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from './client';

// Query keys live here so invalidation and the hooks can't drift apart.
export const keys = {
  server: ['server'] as const,
  stats: ['admin', 'stats'] as const,
  settings: ['admin', 'settings'] as const,
};

export function useServerInfo() {
  return useQuery({ queryKey: keys.server, queryFn: api.serverInfo, staleTime: 5 * 60_000 });
}

/** Catalog totals + who's listening. Polled so "listening now" stays live. */
export function useStats() {
  return useQuery({ queryKey: keys.stats, queryFn: api.stats, refetchInterval: 30_000 });
}

export function useSettings() {
  return useQuery({ queryKey: keys.settings, queryFn: api.settings });
}

export function useScanLibrary() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => api.scanLibrary(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: keys.stats }),
  });
}
