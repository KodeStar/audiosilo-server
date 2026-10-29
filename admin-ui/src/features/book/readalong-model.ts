import type { AdminBook, BridgeJob, ReadalongEstimate, ServerInfo } from '@/api/types';
import { isOpen } from '@/features/bridge/bridge-model';
import {
  coverageDown,
  FEATURE_READALONG,
  KIND_DETECT,
  KIND_GENERATE,
  latestJobOf,
} from '@/features/bridge/generate-model';

// The book page's Read-along card (Bridge Phase 3c): the book's read-along map
// (Book.readalong: where each sentence of its ebook is spoken in its audio),
// and making it with the Bridge's readalong.align job, per book only. Pure: no
// React, no i18n instance; callers pass the keys these return to t().

/**
 * Whether "Make read-along" can be offered: an online Bridge offers the
 * readalong family (it aligns with speech recognition, no agent and no
 * community metadata).
 */
export function readalongAvailable(server: ServerInfo | undefined): boolean {
  if (!server?.capabilities.bridge) return false;
  return server.bridge?.features.includes(FEATURE_READALONG) ?? false;
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
  return READALONG_BLOCKER_KINDS.map((k) => latestJobOf(jobs, k)).find(
    (j): j is BridgeJob => j !== undefined && blocksReadalong(j),
  );
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
  available: boolean,
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
  if (available && book.ebook && !blocker && !(job && isOpen(job.state))) {
    view.action = r ? 'remake' : 'make';
  }
  return view;
}

/** Whether the card shows at all: the book has an ebook, and a map, an offer or a job to show. */
export function readalongCardShown(
  book: Pick<AdminBook, 'ebook' | 'readalong'>,
  view: ReadalongView,
  job: Pick<BridgeJob, 'state'> | undefined,
): boolean {
  if (!book.ebook) return false;
  return view.map !== 'none' || view.action !== undefined || view.blocker !== undefined || !!job;
}

/** What the confirmation says and allows, from the read-along estimate. */
export interface ReadalongDialogView {
  /** The Bridge can't write to the book's library: the server refuses the job. */
  notWritable: boolean;
  /** The job can be queued: the library is writable and the book has an ebook. */
  canConfirm: boolean;
}

export function readalongDialogView(
  e: Pick<ReadalongEstimate, 'writable' | 'ebook'>,
): ReadalongDialogView {
  return { notWritable: !e.writable, canConfirm: e.writable && e.ebook !== null };
}
