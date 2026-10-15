import { keepPreviousData, useInfiniteQuery, useQuery } from '@tanstack/react-query';
import { api } from '@/api/client';
import { keys } from '@/api/hooks';
import { jobsPollMs, statusPollMs } from './bridge-model';

/**
 * The Bridge's pairing and last report, asked again every 15 s while the page
 * is open, and every 3 s while a pairing code waits so the page sees the
 * Bridge take it.
 */
export function useBridge() {
  return useQuery({
    queryKey: keys.bridge,
    queryFn: api.bridge,
    refetchInterval: (q) => statusPollMs(q.state.data),
  });
}

/**
 * The Bridge's jobs, newest first, a page at a time (`fetchNextPage` for older
 * ones), maybe in one state. Asked again every 3 s while any loaded job is open,
 * else every 30 s.
 */
export function useBridgeJobs(state: string) {
  return useInfiniteQuery({
    queryKey: keys.bridgeJobs(state),
    queryFn: ({ pageParam }) =>
      api.bridgeJobs({ state: state || undefined, before: pageParam, limit: 30 }),
    initialPageParam: undefined as number | undefined,
    getNextPageParam: (last) => last.next_before || undefined,
    placeholderData: keepPreviousData,
    refetchInterval: (q) => jobsPollMs(q.state.data?.pages.flatMap((p) => p.jobs) ?? []),
  });
}
