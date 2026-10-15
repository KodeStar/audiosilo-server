import {
  keepPreviousData,
  useInfiniteQuery,
  useQuery,
  useQueryClient,
} from '@tanstack/react-query';
import { api } from '@/api/client';
import { keys } from '@/api/hooks';
import { jobRows, jobsPollMs, statusPollMs } from './bridge-model';

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

const JOBS_PAGE = 30;

/**
 * The Bridge's jobs, newest first, a page at a time (`fetchOlder` for older
 * ones), maybe in one state. Only the newest page is asked again: every 3 s
 * while any job on it is open (open jobs are always the newest), else every
 * 30 s. Older pages stay as loaded until a queue or cancel refreshes the lists.
 */
export function useBridgeJobs(state: string) {
  const qc = useQueryClient();
  const newestKey = keys.bridgeNewestJobs(state);
  const ask = (before?: number) =>
    api.bridgeJobs({ state: state || undefined, before, limit: JOBS_PAGE });

  const newest = useQuery({
    queryKey: newestKey,
    queryFn: () => ask(),
    refetchInterval: (q) => jobsPollMs(q.state.data?.jobs ?? []),
  });
  // Every loaded page, never polled. The first comes through the newest page's
  // query, so the two share one request.
  const pages = useInfiniteQuery({
    queryKey: keys.bridgeJobs(state),
    queryFn: ({ pageParam }) =>
      pageParam === undefined
        ? qc.fetchQuery({ queryKey: newestKey, queryFn: () => ask() })
        : ask(pageParam),
    initialPageParam: undefined as number | undefined,
    getNextPageParam: (last) => last.next_before || undefined,
    placeholderData: keepPreviousData,
  });

  // While another state's pages stand in, the newest page isn't theirs.
  const top = pages.isPlaceholderData ? undefined : newest.data;
  return {
    rows: jobRows(top, pages.data?.pages ?? []),
    isPending: pages.isPending,
    isError: pages.isError || newest.isError,
    error: pages.error ?? newest.error,
    /** Asks for every loaded page again (both queries sit under this key). */
    refetch: () => qc.invalidateQueries({ queryKey: keys.bridgeJobs(state) }),
    hasOlder: (top ? top.next_before !== undefined : true) && pages.hasNextPage,
    fetchOlder: () => pages.fetchNextPage(),
    isFetchingOlder: pages.isFetchingNextPage,
  };
}
