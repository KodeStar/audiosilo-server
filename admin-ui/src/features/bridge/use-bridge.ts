import { useCallback, useState } from 'react';
import {
  keepPreviousData,
  useInfiniteQuery,
  useQuery,
  useQueryClient,
} from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { api } from '@/api/client';
import { keys } from '@/api/hooks';
import type { BridgeStatus } from '@/api/types';
import { toastError } from '@/lib/errors';
import { jobsPollMs, statusPollMs } from './bridge-model';
import type { ShownCode } from './pair-dialog';

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
 * A pairing code for the PairDialog (Settings > Bridge, the setup flow's Bridge
 * step): `makeCode` asks for one, noting the pairing it would replace so the
 * dialog can tell when a Bridge takes it; a refusal is toasted. The status is
 * asked again either way.
 */
export function usePairCode(status: BridgeStatus | undefined) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [shown, setShown] = useState<ShownCode | null>(null);
  const [making, setMaking] = useState(false);
  const makeCode = async () => {
    setMaking(true);
    try {
      const code = await api.pairBridge();
      setShown({
        ...code,
        pairedAtBefore: status?.paired ? status.paired_at : undefined,
        replacing: Boolean(status?.paired),
      });
    } catch (err) {
      toastError(t('bridge.pair.failed'), err);
    } finally {
      setMaking(false);
      void qc.invalidateQueries({ queryKey: keys.bridge });
    }
  };
  const closeCode = useCallback(() => setShown(null), []);
  return { shown, making, makeCode, closeCode };
}

const JOBS_PAGE = 30;

/**
 * The Bridge's jobs, newest first, a page at a time (`fetchOlder` for older
 * ones), maybe in one state. Every loaded page is asked again, every 3 s while
 * any job on them is open, else every 30 s: an infinite query refetches its
 * pages one after another from fresh cursors, so a job still running on an
 * older page stays current (a requeued job keeps its old id, and claims take the
 * oldest queued first), and none is listed twice or skipped when new jobs push
 * the pages along.
 */
export function useBridgeJobs(state: string) {
  const pages = useInfiniteQuery({
    queryKey: keys.bridgeJobs(state),
    queryFn: ({ pageParam }) =>
      api.bridgeJobs({ state: state || undefined, before: pageParam, limit: JOBS_PAGE }),
    initialPageParam: undefined as number | undefined,
    getNextPageParam: (last) => last.next_before || undefined,
    placeholderData: keepPreviousData,
    refetchInterval: (q) => jobsPollMs(q.state.data?.pages.flatMap((p) => p.jobs) ?? []),
  });
  return {
    rows: pages.data?.pages.flatMap((p) => p.jobs) ?? [],
    isPending: pages.isPending,
    isError: pages.isError,
    error: pages.error,
    refetch: () => pages.refetch(),
    hasOlder: pages.hasNextPage,
    fetchOlder: () => pages.fetchNextPage(),
    isFetchingOlder: pages.isFetchingNextPage,
  };
}
