import type { AdminBook, BridgeJob, ReadalongEstimate, ServerInfo } from '@/api/types';
import { isOpen } from '@/features/bridge/bridge-model';
import {
  cancelledView,
  coverageDown,
  FEATURE_READALONG,
  generateResult,
  KIND_DETECT,
  KIND_GENERATE,
  newestBlocker,
} from '@/features/bridge/generate-model';

// The book page's Read-along card (Bridge Phase 3c): the book's read-along map
// (Book.readalong: where each sentence of its ebook is spoken in its audio),
// and making it with the Bridge's readalong.align job, per book only. Pure: no
// React, no i18n instance; callers pass the keys these return to t().

/**
 * The oldest Bridge release that makes read-alongs: the console names it to
 * an admin whose Bridge is older (online, without the readalong family).
 */
export const READALONG_BRIDGE_VERSION = '0.5.0';

/**
 * Where the Bridge stands for making a read-along: `ready` (online, offering
 * the readalong family: it aligns with speech recognition, no agent and no
 * community metadata), `too_old` (online, an older release without it),
 * `offline` (paired, its heartbeat late), `none` (no Bridge paired) or
 * `unknown` (none online, and whether one is paired not known: the server or
 * the Bridge status not loaded yet, or failed), which claims neither.
 * `paired` is the Bridge status's (GET /admin/bridge), read only while no
 * Bridge is online.
 */
export type ReadalongBridge = 'ready' | 'too_old' | 'offline' | 'none' | 'unknown';

export function readalongBridge(
  server: ServerInfo | undefined,
  paired: boolean | undefined,
): ReadalongBridge {
  if (!server) return 'unknown';
  if (server.capabilities.bridge) {
    if (!server.bridge) return 'offline';
    return server.bridge.features.includes(FEATURE_READALONG) ? 'ready' : 'too_old';
  }
  if (server.capabilities.bridge === false && paired === undefined) return 'unknown';
  return paired ? 'offline' : 'none';
}

/**
 * Whether the page should follow the book's read-along jobs: a server that
 * knows the Bridge (on or off), for a book with an ebook (the job needs one).
 */
export function readalongJobsKnown(
  server: ServerInfo | undefined,
  book: Pick<AdminBook, 'ebook'>,
): boolean {
  return server?.capabilities.bridge !== undefined && Boolean(book.ebook);
}

/** The kinds whose open job refuses a new read-along job (the server's generate_open, detect_open). */
const READALONG_BLOCKER_KINDS: readonly string[] = [KIND_GENERATE, KIND_DETECT];

/**
 * Whether a job keeps a read-along job from being queued for its book: a
 * generate or chapter detection job open in any state, parked too (a read-along
 * cancels neither: they run over the same audio), unless it is being cancelled.
 */
export function blocksReadalong(j: Pick<BridgeJob, 'kind' | 'state' | 'cancel_requested'>) {
  return READALONG_BLOCKER_KINDS.includes(j.kind) && isOpen(j.state) && !j.cancel_requested;
}

/** The book's newest job that keeps a read-along from being queued now, if any. */
export function readalongBlocker(jobs: readonly BridgeJob[] | undefined): BridgeJob | undefined {
  return newestBlocker(jobs, READALONG_BLOCKER_KINDS, blocksReadalong);
}

/** What the book has: a map that is ready, one made for another ebook or audio, or none. */
export type ReadalongMapState = 'ready' | 'stale' | 'none';

/** What the Read-along card shows. */
export interface ReadalongView {
  map: ReadalongMapState;
  /**
   * A ready map's share of the ebook's text lined up, rounded down to a whole
   * percent (0.996 reads 99%, never 100%), as a fraction.
   */
  coverage?: number;
  /** When a ready map was made. */
  generatedAt?: string;
  /** A stale map's sentence key (book.readalong.stale.<reason>). */
  staleKey?: string;
  /** The offer: Make, or Make again over a map the book has; none while it can't be queued. */
  action?: 'make' | 'remake';
  /** The open generate or detect job it waits for (its status shows instead of the offer). */
  blocker?: BridgeJob;
  /**
   * No offer because of the Bridge (ADMIN-03, S265-DIALOG): what the card says
   * instead, with a link to Settings > Bridge. Only where it helps: a stale map
   * or a failed job (it needs making again), or a Bridge too old to make one;
   * never while it isn't known whether one is paired.
   */
  bridgeNote?: Exclude<ReadalongBridge, 'ready' | 'unknown'>;
}

/** The stale reasons with a sentence of their own (the rest read as the generic one). */
const STALE_REASONS = ['ebook_changed', 'audio_changed'];

/**
 * The Read-along card for a book: its map (Book.readalong), and the offer,
 * only while a Bridge that makes read-alongs is online (`available`), the book
 * has an ebook, its read-along job isn't open (its status shows; a parked one
 * waits for someone on the Bridge), and no generate or detect job holds it.
 */
export function readalongView(
  book: Pick<AdminBook, 'ebook' | 'readalong'>,
  job: Pick<BridgeJob, 'state'> | undefined,
  jobs: readonly BridgeJob[] | undefined,
  bridge: ReadalongBridge,
): ReadalongView {
  const r = book.readalong;
  const view: ReadalongView = { map: r?.state ?? 'none' };
  if (r?.state === 'ready') {
    if (r.coverage !== undefined) view.coverage = coverageDown(r.coverage);
    if (r.generated_at) view.generatedAt = r.generated_at;
  } else if (r?.state === 'stale') {
    view.staleKey = `book.readalong.stale.${STALE_REASONS.includes(r.reason ?? '') ? r.reason : 'other'}`;
  }
  const blocker = readalongBlocker(jobs);
  if (blocker) view.blocker = blocker;
  if (!book.ebook || blocker || (job && isOpen(job.state))) return view;
  if (bridge === 'ready') {
    view.action = r ? 'remake' : 'make';
  } else if (
    bridge !== 'unknown' &&
    (bridge === 'too_old' || r?.state === 'stale' || job?.state === 'failed')
  ) {
    view.bridgeNote = bridge;
  }
  return view;
}

/**
 * Whether the card shows the book's last read-along job under the map: an
 * open, parked or failed one, and a cancelled one the server gave a reason
 * for. A done job shows only when it says more than the map: not over a ready
 * map it made (the map's line already says how much lined up), unless it
 * reused a transcript, and never over a stale one (that map is out of date).
 */
export function readalongJobShown(
  map: ReadalongMapState,
  job: Pick<BridgeJob, 'state' | 'error' | 'cancel_requested' | 'progress' | 'result'> | undefined,
): boolean {
  if (!job) return false;
  if (job.state === 'cancelled') return cancelledView(job).reason !== undefined;
  if (job.state !== 'done') return true;
  if (map === 'stale') return false;
  return map !== 'ready' || generateResult(job.result)?.transcribed === false;
}

/** Whether the card shows at all: the book has an ebook, and a map, an offer or a job to show. */
export function readalongCardShown(
  book: Pick<AdminBook, 'ebook' | 'readalong'>,
  view: ReadalongView,
  job: Pick<BridgeJob, 'state'> | undefined,
): boolean {
  if (!book.ebook) return false;
  return (
    view.map !== 'none' ||
    view.action !== undefined ||
    view.blocker !== undefined ||
    view.bridgeNote !== undefined ||
    !!job
  );
}

/** What the confirmation says and allows, from the read-along estimate. */
export interface ReadalongDialogView {
  /** The Bridge can't write to the book's library: the server refuses the job. */
  notWritable: boolean;
  /**
   * The job can be queued: the library is writable, the book has an ebook, and
   * the Bridge isn't one too old to make it (the server would refuse the kind).
   * An offline Bridge doesn't stop it: the job waits for it.
   */
  canConfirm: boolean;
}

export function readalongDialogView(
  e: Pick<ReadalongEstimate, 'writable' | 'ebook'>,
  bridge: ReadalongBridge,
): ReadalongDialogView {
  return {
    notWritable: !e.writable,
    canConfirm: e.writable && e.ebook !== null && bridge !== 'too_old',
  };
}
