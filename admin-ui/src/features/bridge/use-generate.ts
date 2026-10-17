import { useEffect, useRef } from 'react';
import { queryOptions, useQuery, useQueryClient, type QueryClient } from '@tanstack/react-query';
import type { TFunction } from 'i18next';
import { api, ApiError } from '@/api/client';
import { keys } from '@/api/hooks';
import type { BridgeJob, BridgeJobRequest } from '@/api/types';
import { errorMessage } from '@/lib/errors';
import { formatList, formatNumber } from '@/lib/format';
import { refKey } from '@/lib/book-route';
import { relBaseName } from '@/lib/paths';
import { isOpen, kindName } from './bridge-model';
import { generatePollMs, KIND_DETECT, KIND_GENERATE, latestJobOf } from './generate-model';

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

/**
 * A book a run passed over, and why: `parked` (the open job that came back is
 * parked, waiting for someone on the Bridge, so nothing was queued) or the
 * server's refusal code the run was told to pass over (`skip`).
 */
export interface QueueSkip {
  request: BridgeJobRequest;
  reason: string;
}

/**
 * How a run of queued jobs went: how many were queued before the first failure,
 * the books passed over, and that failure with the request it stopped at.
 */
export interface QueueOutcome {
  queued: number;
  skipped: QueueSkip[];
  error?: unknown;
  failed?: BridgeJobRequest;
}

/**
 * The open job that came back for a book is one being cancelled: it ends without
 * running, and nothing new is queued until it has stopped (as the Bridge page's
 * queueJob says).
 */
class StillStopping extends Error {
  constructor() {
    super('still stopping');
  }
}

/**
 * Queues the jobs one after another, in order (ids ascend, so the Bridge claims
 * them in that order), stopping at the first failure: the ones after it would
 * run out of order, and most failures (a library the Bridge can't write, a kind
 * it no longer offers) would refuse them all. A job already open for a book
 * comes back instead of a new one, which counts as queued, unless it is parked
 * (nothing was queued: the book is passed over as `parked`, waiting for someone)
 * or being cancelled: that book would be left without one, so the run stops
 * there. A refusal whose code is in `skip` passes the book over instead (Detect
 * chapters' book whose generate job is open, a generate for a book whose
 * chapters are being found); `skipNotes` words them.
 */
export async function queueInOrder(
  qc: QueryClient,
  requests: readonly BridgeJobRequest[],
  { skip = [] }: { skip?: readonly string[] } = {},
): Promise<QueueOutcome> {
  let queued = 0;
  const skipped: QueueSkip[] = [];
  let req: BridgeJobRequest | undefined;
  try {
    for (req of requests) {
      try {
        const { job, created } = await api.queueBridgeJob(req);
        if (!created && job.state === 'needs_attention') {
          skipped.push({ request: req, reason: 'parked' });
          continue;
        }
        if (!created && job.cancel_requested) throw new StillStopping();
        queued++;
      } catch (err) {
        const code = err instanceof ApiError ? err.code : undefined;
        if (!code || !skip.includes(code)) throw err;
        skipped.push({ request: req, reason: code });
      }
    }
    return { queued, skipped };
  } catch (error) {
    return { queued, skipped, error, failed: req };
  } finally {
    void qc.invalidateQueries({ queryKey: keys.bridgeJobLists });
    void qc.invalidateQueries({ queryKey: keys.bridgeEstimates });
  }
}

/**
 * The refusals a generate run passes a book over for: its chapters are being
 * found (a queued or working chapters.detect job; the generate job is queued once
 * that has finished, reusing its transcription).
 */
export const GENERATE_SKIPS = ['detect_open'] as const;

/** How many books a skip note names before it counts the rest. */
const NAMED = 5;

/** The skip reasons with words of their own (the rest read as the generic note). */
const SKIP_KEYS: Record<string, string> = {
  parked: 'bridge.queue.skip.parked',
  generate_open: 'bridge.queue.skip.generateOpen',
  detect_open: 'bridge.queue.skip.detectOpen',
  describe_open: 'bridge.queue.skip.describeOpen',
  chapters_chosen: 'bridge.queue.skip.chaptersChosen',
};

/**
 * The books a run passed over, one sentence per reason naming them (the first
 * few, then how many more), in the order the reasons first came up. `nameOf`
 * names a request's book (its folder's name when not given).
 */
export function skipNotes(
  out: Pick<QueueOutcome, 'skipped'>,
  t: TFunction,
  lang: string,
  nameOf: (req: BridgeJobRequest) => string = (req) => relBaseName(req.path ?? ''),
): string[] {
  const byReason = new Map<string, string[]>();
  for (const s of out.skipped) {
    const names = byReason.get(s.reason) ?? [];
    names.push(nameOf(s.request));
    byReason.set(s.reason, names);
  }
  return [...byReason].map(([reason, names]) => {
    const shown =
      names.length > NAMED
        ? [
            ...names.slice(0, NAMED - 1),
            t('bridge.queue.skip.more', { n: formatNumber(names.length - NAMED + 1, lang) }),
          ]
        : names;
    return t(SKIP_KEYS[reason] ?? 'bridge.queue.skip.other', { names: formatList(shown, lang) });
  });
}

/**
 * A queued request's book by the title `titles` (keyed by refKey) has for it,
 * else its folder's name.
 */
export function requestTitle(titles: ReadonlyMap<string, string>, req: BridgeJobRequest): string {
  const path = req.path ?? '';
  return titles.get(refKey({ library_id: req.library_id ?? 0, path })) || relBaseName(path);
}

/** What a Bridge that no longer offers a kind means in the dialogs that queue it. */
const KIND_UNAVAILABLE: Record<string, string> = {
  [KIND_GENERATE]: 'bridge.generate.kindUnavailable',
  [KIND_DETECT]: 'bridge.detect.kindUnavailable',
};

/**
 * A queue failure in the dialogs' words: a Bridge that no longer offers the
 * kind is named for what it means there; the rest (a library the Bridge can't
 * write among them) as anywhere else.
 */
function queueError(out: QueueOutcome, t: TFunction): string {
  const kind = out.failed?.kind ?? KIND_GENERATE;
  if (
    out.error instanceof ApiError &&
    out.error.code === 'kind_unavailable' &&
    KIND_UNAVAILABLE[kind]
  )
    return t(KIND_UNAVAILABLE[kind]);
  if (out.error instanceof StillStopping)
    return t('bridge.queue.stopping', {
      what: kindName(t, kind),
      name: relBaseName(out.failed?.path ?? ''),
    });
  return errorMessage(out.error, t);
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
  const why = queueError(out, t);
  return out.queued
    ? t('bridge.generate.partial', {
        done: formatNumber(out.queued, lang),
        total: formatNumber(total, lang),
        error: why,
      })
    : why;
}
