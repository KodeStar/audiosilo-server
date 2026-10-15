import { useEffect, useRef } from 'react';
import { useQuery, useQueryClient, type QueryClient } from '@tanstack/react-query';
import type { TFunction } from 'i18next';
import { api, ApiError } from '@/api/client';
import { keys } from '@/api/hooks';
import type { BridgeJob, BridgeJobRequest } from '@/api/types';
import { errorMessage } from '@/lib/errors';
import { isOpen } from './bridge-model';
import { generatePollMs, justFinished, KIND_GENERATE, latestGenerateJob } from './generate-model';

/** How many of a book's newest jobs are read to find its generate job (verify jobs share the list). */
const BOOK_JOBS = 20;

/**
 * A book's newest `sidecars.generate` job, asked again about every 10 s while
 * it is open and only while the page showing it is mounted (TanStack Query
 * polls for mounted observers only), then not at all. When it moves from open
 * to done, the book, its estimate and its community metadata are fetched again,
 * so the page shows the new local data.
 */
export function useBookGenerateJob(libraryId: number, path: string, enabled: boolean) {
  const qc = useQueryClient();
  const query = useQuery({
    queryKey: keys.bookBridgeJobs(libraryId, path),
    queryFn: () => api.bridgeJobs({ library_id: libraryId, path, limit: BOOK_JOBS }),
    enabled,
    refetchInterval: (q) => generatePollMs(q.state.data?.jobs),
  });
  const job = latestGenerateJob(query.data?.jobs);
  const seen = useRef<BridgeJob | undefined>(undefined);
  useEffect(() => {
    const before = seen.current;
    seen.current = job;
    if (!before || !job || before.id !== job.id) return;
    if (isOpen(before.state) && !isOpen(job.state)) {
      // What the book has, and whether a job is open, changed with it.
      void qc.invalidateQueries({ queryKey: keys.bridgeEstimate(libraryId, path) });
    }
    if (justFinished(before, job)) {
      void qc.invalidateQueries({ queryKey: keys.book(libraryId, path), exact: true });
      void qc.invalidateQueries({ queryKey: keys.bookMeta(libraryId, path) });
    }
  }, [job, qc, libraryId, path]);
  return { job, isPending: query.isPending && enabled };
}

/**
 * What generating would take for a book (the source, ASR hours, the expected
 * agent cost) and what it has already. It also decides the card's action
 * (Generate, Regenerate, or none when the community has both), so it is asked
 * when the page opens and kept for a minute.
 */
export function useGenerateEstimate(
  libraryId: number,
  path: string,
  enabled: boolean,
  /** Ask again on mount however fresh it is (the confirmation dialog, which quotes it). */
  fresh = false,
) {
  return useQuery({
    queryKey: keys.bridgeEstimate(libraryId, path),
    queryFn: () => api.bridgeEstimate(libraryId, path, KIND_GENERATE),
    enabled,
    retry: false,
    staleTime: 60_000,
    refetchOnMount: fresh ? 'always' : true,
  });
}

/**
 * The Bridge's status for a link into its page (ui_url), read once while a
 * parked job needs it. It shares the Bridge page's query; this observer adds no
 * polling of its own.
 */
export function useBridgeLink(enabled: boolean) {
  const q = useQuery({ queryKey: keys.bridge, queryFn: api.bridge, enabled, staleTime: 60_000 });
  return q.data?.ui_url;
}

/** How a run of queued jobs went: how many were queued before the first failure, and that failure. */
export interface QueueOutcome {
  queued: number;
  error?: unknown;
}

/**
 * Queues the jobs one after another, in order (ids ascend, so the Bridge claims
 * them in that order), stopping at the first failure: the ones after it would
 * run out of order, and most failures (a library the Bridge can't write, a kind
 * it no longer offers) would refuse them all. A job already open for a book
 * comes back instead of a new one, which counts as queued.
 */
export async function queueInOrder(
  qc: QueryClient,
  requests: readonly BridgeJobRequest[],
): Promise<QueueOutcome> {
  let queued = 0;
  try {
    for (const req of requests) {
      await api.queueBridgeJob(req);
      queued++;
    }
    return { queued };
  } catch (error) {
    return { queued, error };
  } finally {
    void qc.invalidateQueries({ queryKey: keys.bridgeJobLists });
    void qc.invalidateQueries({ queryKey: keys.bridgeEstimates });
  }
}

/**
 * A queue failure in the generate dialogs' words: a Bridge that no longer
 * offers the kind is named for what it means here; the rest (a library the
 * Bridge can't write among them) as anywhere else.
 */
export function generateError(err: unknown, t: TFunction): string {
  if (err instanceof ApiError && err.code === 'kind_unavailable')
    return t('bridge.generate.kindUnavailable');
  return errorMessage(err, t);
}
