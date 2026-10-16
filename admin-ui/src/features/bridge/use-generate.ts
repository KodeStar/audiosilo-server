import { useEffect, useRef } from 'react';
import { queryOptions, useQuery, useQueryClient, type QueryClient } from '@tanstack/react-query';
import type { TFunction } from 'i18next';
import { api, ApiError } from '@/api/client';
import { keys } from '@/api/hooks';
import type { BridgeJob, BridgeJobRequest } from '@/api/types';
import { errorMessage } from '@/lib/errors';
import { formatNumber } from '@/lib/format';
import { relBaseName } from '@/lib/paths';
import { isOpen, kindName } from './bridge-model';
import { generatePollMs, KIND_GENERATE, latestJobOf } from './generate-model';

/**
 * How many of a book's newest jobs are read to find its generate and describe
 * jobs, and whether either is open (verify jobs share the list).
 */
const BOOK_JOBS = 20;

/**
 * A book's newest job of a kind (`sidecars.generate` unless told otherwise),
 * and the book's newest jobs, asked again about every 10 s while a generate or
 * describe job works and every minute while one is parked, only while the page
 * showing it is mounted (TanStack Query polls for mounted observers only), then
 * not at all. Every kind shares one query. When the job ends, the book, its
 * estimates and its community metadata are fetched again, so the page shows
 * what changed.
 */
export function useBookGenerateJob(
  libraryId: number,
  path: string,
  enabled: boolean,
  kind: string = KIND_GENERATE,
) {
  const qc = useQueryClient();
  const query = useQuery({
    queryKey: keys.bookBridgeJobs(libraryId, path),
    queryFn: () => api.bridgeJobs({ library_id: libraryId, path, limit: BOOK_JOBS }),
    enabled,
    refetchInterval: (q) => generatePollMs(q.state.data?.jobs),
  });
  const job = latestJobOf(query.data?.jobs, kind);
  const seen = useRef<BridgeJob | undefined>(undefined);
  useEffect(() => {
    const before = seen.current;
    seen.current = job;
    if (!before || !job || before.id !== job.id) return;
    if (!isOpen(before.state) || isOpen(job.state)) return;
    // What the book has, and whether a job is open, changed with it.
    void qc.invalidateQueries({ queryKey: keys.bridgeEstimate(libraryId, path) });
    void qc.invalidateQueries({ queryKey: keys.book(libraryId, path), exact: true });
    void qc.invalidateQueries({ queryKey: keys.bookMeta(libraryId, path) });
  }, [job, qc, libraryId, path]);
  return { job, jobs: query.data?.jobs };
}

/**
 * A book's generate estimate. The full one (the confirmation dialog) holds the
 * source, the earlier books of the series and the cost; the `brief` one leaves
 * out the costly look-ups (`earlier: []`, `source: "audio"`) but still says
 * what the book has, whether a job is open, whether the library is writable,
 * and the cost figures: enough for the card's action and the series batch.
 * Each has its own key.
 */
export function estimateQuery(libraryId: number, path: string, { brief }: { brief: boolean }) {
  return queryOptions({
    queryKey: [...keys.bridgeEstimate(libraryId, path), brief ? 'brief' : 'full'] as const,
    queryFn: () => api.bridgeEstimate(libraryId, path, KIND_GENERATE, brief),
    retry: false,
    staleTime: 60_000,
  });
}

/**
 * What generating would take for a book, and what it has already. The card
 * asks for the brief estimate when the page opens (it decides Generate,
 * Regenerate, or none when the community has both) and keeps it a minute; the
 * confirmation dialog asks for the full one each time it opens, since it quotes it.
 */
export function useGenerateEstimate(
  libraryId: number,
  path: string,
  enabled: boolean,
  { brief }: { brief: boolean },
) {
  return useQuery({
    ...estimateQuery(libraryId, path, { brief }),
    enabled,
    refetchOnMount: brief ? true : 'always',
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
 * The open job that came back for a book is one being cancelled: it ends without
 * running, and nothing new is queued until it has stopped (as the Bridge page's
 * queueJob says).
 */
class StillStopping extends Error {
  readonly path: string;
  constructor(path: string) {
    super('still stopping');
    this.path = path;
  }
}

/**
 * Queues the jobs one after another, in order (ids ascend, so the Bridge claims
 * them in that order), stopping at the first failure: the ones after it would
 * run out of order, and most failures (a library the Bridge can't write, a kind
 * it no longer offers) would refuse them all. A job already open for a book
 * comes back instead of a new one, which counts as queued, unless it is being
 * cancelled: that book would be left without one, so the run stops there.
 */
export async function queueInOrder(
  qc: QueryClient,
  requests: readonly BridgeJobRequest[],
): Promise<QueueOutcome> {
  let queued = 0;
  try {
    for (const req of requests) {
      const { job, created } = await api.queueBridgeJob(req);
      if (!created && job.cancel_requested) throw new StillStopping(req.path ?? '');
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
function generateError(err: unknown, t: TFunction): string {
  if (err instanceof ApiError && err.code === 'kind_unavailable')
    return t('bridge.generate.kindUnavailable');
  if (err instanceof StillStopping)
    return t('bridge.queue.stopping', {
      what: kindName(t, KIND_GENERATE),
      name: relBaseName(err.path),
    });
  return errorMessage(err, t);
}

/**
 * The dialogs' error line for a run of `total` queued jobs, or undefined when
 * every one was queued: the failure, and how many went before it when some did.
 */
export function queueReport(
  out: QueueOutcome,
  total: number,
  t: TFunction,
  lang: string,
): string | undefined {
  if (out.error === undefined) return undefined;
  const why = generateError(out.error, t);
  return out.queued
    ? t('bridge.generate.partial', {
        done: formatNumber(out.queued, lang),
        total: formatNumber(total, lang),
        error: why,
      })
    : why;
}
