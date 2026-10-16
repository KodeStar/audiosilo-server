import type {
  AdminBook,
  BridgeEstimate,
  BridgeJob,
  BridgeJobRequest,
  ServerInfo,
  SidecarsCoverage,
  SidecarsGenerateParams,
} from '@/api/types';
import { refKey } from '@/lib/book-route';
import { relBaseName } from '@/lib/paths';
import { spanIn } from '@/features/library/series/series-model';
import { isOpen, num, progressOf, safeHttpUrl, text } from './bridge-model';

// Characters and recaps from the Bridge (BRIDGE-PLAN.md Phase 1a), and the
// description written from their notes (Phase 1c): the rules the book page's
// Community card, its generate dialog, the series batch and the Bridge page's
// job rows follow for `sidecars.generate` and `sidecars.describe`. Pure: no React, no i18n
// instance; callers pass the keys these return to t().

/** The job kind, and the feature (job family) the Bridge advertises for it. */
export const KIND_GENERATE = 'sidecars.generate';
export const FEATURE_SIDECARS = 'sidecars';
/** Phase 1c's description-only kind: a description from the notes the Bridge kept. */
export const KIND_DESCRIBE = 'sidecars.describe';

/** The kinds the book page follows: generate and describe (both in the sidecars family). */
const BOOK_KINDS: readonly string[] = [KIND_GENERATE, KIND_DESCRIBE];

/**
 * Whether a kind is one the book page follows; its progress and result are
 * worded by GenerateStatus, on the book page and the Bridge page's job rows.
 */
export function isBookKind(kind: string): boolean {
  return BOOK_KINDS.includes(kind);
}

/** The book page's kinds' jobs that are still open. */
function openBookJobs<J extends Pick<BridgeJob, 'kind' | 'state'>>(
  jobs: readonly J[] | undefined,
): J[] {
  return jobs?.filter((j) => isBookKind(j.kind) && isOpen(j.state)) ?? [];
}

/**
 * Whether the Generate action is offered at all: community metadata is on (the
 * generated content is served through /meta, which needs it) and an online
 * Bridge offers the sidecars family.
 */
export function generateAvailable(server: ServerInfo | undefined): boolean {
  if (!server?.capabilities.metadata || !server.capabilities.bridge) return false;
  return server.bridge?.features.includes(FEATURE_SIDECARS) ?? false;
}

/**
 * Whether the book page should ask for its generate jobs at all: a server that
 * knows the Bridge (it reports the capability, on or off) with metadata on. A
 * job queued while the Bridge was online stays worth showing while it's away.
 */
export function generateJobsKnown(server: ServerInfo | undefined): boolean {
  return Boolean(server?.capabilities.metadata && server.capabilities.bridge !== undefined);
}

/** What the card offers: Generate, Regenerate (local data exists), or nothing (the community has both). */
export type GenerateAction = 'generate' | 'regenerate' | 'hidden';

export function generateAction(has: {
  community: SidecarsCoverage;
  local: SidecarsCoverage;
}): GenerateAction {
  if (has.community.characters && has.community.recaps) return 'hidden';
  return has.local.characters || has.local.recaps ? 'regenerate' : 'generate';
}

/**
 * How often a book's generate and describe jobs are asked again: about every
 * 10 s while one is queued or working, every minute while one is parked (it
 * waits on a person or a reset), else not at all.
 */
export const GENERATE_POLL_MS = 10_000;
export const GENERATE_PARKED_POLL_MS = 60_000;

export function generatePollMs(
  jobs: readonly Pick<BridgeJob, 'kind' | 'state'>[] | undefined,
): number | false {
  const open = openBookJobs(jobs);
  if (open.some((j) => j.state !== 'needs_attention')) return GENERATE_POLL_MS;
  return open.length ? GENERATE_PARKED_POLL_MS : false;
}

/** The book's newest job of a kind (the list is newest first), if any. */
export function latestJobOf<J extends Pick<BridgeJob, 'kind'>>(
  jobs: readonly J[] | undefined,
  kind: string,
): J | undefined {
  return jobs?.find((j) => j.kind === kind);
}

/**
 * Whether the card offers "Write a description": the Bridge can (online, runs
 * sidecars.describe), the book has characters or recaps generated here (their
 * notes are what the description is written from), it has no description yet,
 * here or from the community, and no generate or describe job is open for it
 * (a generate job writes one when it finishes).
 */
export function describeOffered(
  e: Pick<BridgeEstimate, 'can_describe' | 'has'> | undefined,
  jobs: readonly Pick<BridgeJob, 'kind' | 'state'>[] | undefined,
): boolean {
  if (!e?.can_describe) return false;
  const { local, community } = e.has;
  if (!(local.characters || local.recaps) || local.description || community.description) {
    return false;
  }
  return openBookJobs(jobs).length === 0;
}

/** A generated description long enough to clamp, with a Show more toggle (about four lines). */
export const DESCRIPTION_CLAMP_CHARS = 240;

export function clampsDescription(text: string): boolean {
  return text.trim().length > DESCRIPTION_CLAMP_CHARS;
}

// The engine's stage names (state.State) and the Bridge's own steps, each with a
// label of its own (bridge.stage.<name>); any other name reads title-cased.
export const KNOWN_STAGES = [
  'queued',
  'inspecting',
  'splitting',
  'asr',
  'sanitizing',
  'qa_sweep',
  'qa_adjudicating',
  'retranscribing',
  'markers_normalizing',
  'spelling_research',
  'correcting',
  'fact_pass',
  'synthesizing',
  'auditing',
  'fixing',
  'validating',
  'ready',
  'contributing',
  'extracting',
  'chapter_mapping',
  'waiting',
  'writing',
  'rescanning',
  'describing',
] as const;

/** "qa_sweep" -> "Qa sweep": a name this console has no label for, made readable. */
export function titleCase(name: string): string {
  const words = name.replace(/[_.-]+/g, ' ').trim();
  return words ? words[0].toUpperCase() + words.slice(1) : '';
}

/** A stage's label key and its fallback (a stage the console doesn't know reads title-cased). */
export function stageLabel(stage: string): { key: string; fallback: string } {
  return { key: `bridge.stage.${stage}`, fallback: titleCase(stage) };
}

// The engine's park codes (state.ParkCode) and the Bridge's own, each with a
// sentence (bridge.park.<code>); any other code is the generic one with the code.
export const KNOWN_PARKS = [
  'agent_unavailable',
  'agent_rate_limited',
  'agent_validation_exhausted',
  'markers_not_confident',
  'qa_no_converge',
  'spelling_gate_failure',
  'media_tools_unavailable',
  'asr_unavailable',
  'manifest_changed',
  'fix_loop_exhausted',
  'asr_decode_timeout',
  'asr_worker_waiting',
  'contrib_unavailable',
  'core_needed',
  'core_pending',
  'ebook_unreadable',
  'ebook_no_chapters',
  'ebook_chapters_not_confident',
  'budget_exceeded',
  'supervisor_escalated',
  'supervisor_budget',
  'book_not_writable',
  'library_not_writable',
  'agent_credential_refused',
] as const;

/** A park code's sentence key: its own, or the generic one (which names the code). */
export function parkKey(code: string): string {
  return (KNOWN_PARKS as readonly string[]).includes(code)
    ? `bridge.park.${code}`
    : 'bridge.park.unknown';
}

/** A parked job's page on the Bridge (`<ui_url>#/jobs/<id>`), only for an http(s) ui_url. */
export function bridgeJobLink(uiUrl: string | undefined, jobId: number): string | undefined {
  const base = safeHttpUrl(uiUrl);
  if (!base) return undefined;
  const u = new URL(base);
  u.hash = `/jobs/${jobId}`;
  return u.href;
}

/** A figure that can't be below zero (a count, an ETA, a cost), else undefined. */
const amount = (v: unknown): number | undefined => {
  const n = num(v);
  return n !== undefined && n >= 0 ? n : undefined;
};

/** A generate job's progress as the card words it; every field only when the Bridge sent it. */
export interface GenerateProgress {
  /** The engine stage, else the step. */
  stage?: string;
  message?: string;
  done?: number;
  total?: number;
  etaSeconds?: number;
  costUsd?: number;
  park?: { code: string; hint?: string; retryAt?: string };
}

/** Reads a sidecars.generate progress leniently (the server stores it opaquely). */
export function generateProgress(progress: unknown): GenerateProgress {
  if (!progress || typeof progress !== 'object') return {};
  const p = progress as Record<string, unknown>;
  // The message (else the step) and the count, as any kind's progress reads.
  const { text: said, done, total } = progressOf(p);
  const step = text(p.step);
  const out: GenerateProgress = {};
  // The Bridge's own last steps (saving the files, the rescan) say what is
  // happening; the engine's stage by then is only "done".
  const stage =
    (step === 'writing' || step === 'rescanning' ? step : undefined) ?? text(p.stage) ?? step;
  if (stage) out.stage = stage;
  // The step alone is no message: it is the stage when there is none. Nor is the
  // Bridge's wording of a stage this console labels (an English restatement of
  // the stage and its count, already shown in the admin's language).
  const labelled = stage !== undefined && (KNOWN_STAGES as readonly string[]).includes(stage);
  if (said && said !== step && !labelled) out.message = said;
  if (done !== undefined && total !== undefined) {
    out.done = done;
    out.total = total;
  }
  const eta = amount(p.eta_seconds);
  if (eta !== undefined) out.etaSeconds = eta;
  const cost = amount(p.cost_usd);
  if (cost !== undefined) out.costUsd = cost;
  if (p.park && typeof p.park === 'object') {
    const k = p.park as Record<string, unknown>;
    const code = text(k.code);
    if (code) {
      out.park = { code };
      const hint = text(k.hint);
      if (hint) out.park.hint = hint;
      const retryAt = text(k.retry_at);
      if (retryAt && !Number.isNaN(Date.parse(retryAt))) out.park.retryAt = retryAt;
    }
  }
  return out;
}

/** A finished generate (or describe) job's result as the card words it. */
export interface GenerateResult {
  characters?: number;
  recaps?: number;
  costUsd?: number;
  /** The description was written (true) or not (false, with why); absent from an older Bridge. */
  description?: boolean;
  descriptionError?: string;
  /** The community issues filed (http(s) links only), by what each offers. */
  contributed: { what: string; url: string }[];
}

/** Reads a sidecars.generate result leniently, or null when it holds none of its facts. */
export function generateResult(result: unknown): GenerateResult | null {
  if (!result || typeof result !== 'object') return null;
  const r = result as Record<string, unknown>;
  const out: GenerateResult = { contributed: [] };
  const characters = amount(r.characters);
  if (characters !== undefined) out.characters = characters;
  const recaps = amount(r.recaps);
  if (recaps !== undefined) out.recaps = recaps;
  const cost = amount(r.cost_usd);
  if (cost !== undefined) out.costUsd = cost;
  if (typeof r.description === 'boolean') out.description = r.description;
  const why = text(r.description_error);
  if (out.description === false && why) out.descriptionError = why;
  if (r.contributed && typeof r.contributed === 'object') {
    for (const [what, url] of Object.entries(r.contributed as Record<string, unknown>)) {
      const safe = typeof url === 'string' ? safeHttpUrl(url) : undefined;
      if (safe) out.contributed.push({ what, url: safe });
    }
  }
  const any =
    out.characters !== undefined ||
    out.recaps !== undefined ||
    out.costUsd !== undefined ||
    out.description !== undefined ||
    out.contributed.length > 0;
  return any ? out : null;
}

/**
 * An ETA rounded to what a person can use: a minute at least, whole minutes
 * under an hour, 5 minutes under ten hours, then half hours.
 */
export function roundEta(seconds: number): number {
  if (!Number.isFinite(seconds) || seconds <= 0) return 0;
  if (seconds < 60) return 60;
  const step = seconds < 3600 ? 60 : seconds < 36_000 ? 300 : 1800;
  return Math.max(step, Math.round(seconds / step) * step);
}

/**
 * The confirmation dialog's estimate, as facts the dialog words: the ebook's
 * file when it reads one, the transcription it needs, the agent cost the
 * Bridge's history predicts, and what stops or warns.
 */
export interface EstimateView {
  ebook?: { name: string; path: string };
  /** Transcription hours; 0 = none needed (an ebook); null = no estimate yet. */
  asrHours: number | null;
  /** The expected agent cost; null = no estimate yet. */
  costUsd: number | null;
  /** The Bridge's agent runs on a subscription: the cost is an estimate of usage. */
  subscription: boolean;
  /** The agent isn't ready: the job parks until it is. */
  agentNotReady: boolean;
  /** The Bridge hasn't sent its figures yet: whether the agent is ready isn't known. */
  noFigures: boolean;
  /** The Bridge can offer the results to the community: the contribute choice is offered. */
  canContribute: boolean;
  /** The Bridge can't write to the library: nothing can be queued. */
  notWritable: boolean;
}

export function estimateView(e: BridgeEstimate): EstimateView {
  const ebook = e.source === 'ebook';
  const asr = amount(e.asr_hours);
  return {
    ...(ebook && e.ebook ? { ebook: { name: relBaseName(e.ebook), path: e.ebook } } : {}),
    asrHours: ebook ? 0 : asr && asr > 0 ? asr : null,
    costUsd: amount(e.agent_cost_usd) ?? null,
    subscription: e.subscription,
    agentNotReady: e.figures && !e.agent_ready,
    noFigures: !e.figures,
    canContribute: e.can_contribute,
    notWritable: !e.writable,
  };
}

/** Where the results appear, by the contribute choice (decision 6: local by default). */
export function destinationKey(contribute: Contribute): string {
  return contribute === 'issue' ? 'bridge.generate.whereIssue' : 'bridge.generate.whereLocal';
}

/** Where generated content goes: this server only, or offered to the community too. */
export type Contribute = NonNullable<SidecarsGenerateParams['contribute']>;

/**
 * The jobs queued for books already in reading order (the book dialog's
 * earlier books then the book, or the series batch's queue), each book once.
 * Ids ascend, so the Bridge claims them in that order.
 */
export function generatePlan(
  books: readonly { library_id: number; path: string }[],
  contribute: Contribute,
): BridgeJobRequest[] {
  const params: SidecarsGenerateParams = { source: 'auto', contribute };
  const seen = new Set<string>();
  const out: BridgeJobRequest[] = [];
  for (const b of books) {
    const key = refKey(b);
    if (seen.has(key)) continue;
    seen.add(key);
    out.push({
      kind: KIND_GENERATE,
      library_id: b.library_id,
      path: b.path,
      params: { ...params },
    });
  }
  return out;
}

/**
 * A series' books in reading order for the batch: by their position in this
 * series (each spelling of it), books without one last in the order they came,
 * each book once.
 */
export function seriesBatchOrder<B extends AdminBook>(books: readonly B[], series: string): B[] {
  const seen = new Set<string>();
  const unique = books.filter((b) => {
    const key = refKey(b);
    if (seen.has(key)) return false;
    seen.add(key);
    return true;
  });
  // Its position in this series; 0 when it has none.
  const pos = (b: B) => spanIn(b, series).position;
  return unique
    .map((b, i) => ({ b, i, p: pos(b) }))
    .sort((x, y) => {
      const xa = x.p > 0;
      const ya = y.p > 0;
      if (xa !== ya) return xa ? -1 : 1;
      return (xa && x.p !== y.p ? x.p - y.p : 0) || x.i - y.i;
    })
    .map((x) => x.b);
}

/** What the series batch knows of a book once its estimate answered (or failed). */
export interface BatchCheck {
  /** It has recaps already, from the community or its own folder: skipped. */
  covered: boolean;
  /** A generate job is already open for it (and not being cancelled): skipped. */
  open: boolean;
  /**
   * It couldn't be checked (its estimate failed: a book gone since the list was
   * read, the Bridge away; or the community couldn't be asked): skipped rather
   * than queued on a guess, since the community may have its recaps.
   */
  unknown: boolean;
  /** The Bridge can't write to its library. */
  notWritable: boolean;
  /** Its expected agent cost; null when there is no estimate. */
  costUsd: number | null;
  /** The Bridge's agent is on a subscription: the cost is an estimate of usage. */
  subscription: boolean;
  /** The Bridge can offer the results to the community. */
  canContribute: boolean;
}

export function batchCheck(e: BridgeEstimate | undefined): BatchCheck {
  if (!e) {
    return {
      covered: false,
      open: false,
      unknown: true,
      notWritable: false,
      costUsd: null,
      subscription: false,
      canContribute: false,
    };
  }
  const covered = e.has.community.recaps || e.has.local.recaps;
  return {
    covered,
    // A job being cancelled ends without running, so it doesn't cover the book:
    // queueing it again says it is still stopping (queueInOrder).
    open: Boolean(e.open_job && isOpen(e.open_job.state) && !e.open_job.cancel_requested),
    unknown: !covered && Boolean(e.has.community_unknown),
    notWritable: !e.writable,
    costUsd: amount(e.agent_cost_usd) ?? null,
    subscription: e.subscription,
    canContribute: e.can_contribute,
  };
}

/** The series batch's plan: the books to queue (in order), what they cost in all, and how many were skipped and why. */
export interface BatchPlan<B> {
  queue: B[];
  covered: number;
  open: number;
  unknown: number;
  /** A book to be queued is in a library the Bridge can't write to: nothing can be. */
  notWritable: boolean;
  /** The queued books' agent cost in all; null when any has no estimate (or none is queued). */
  costUsd: number | null;
  subscription: boolean;
  /** Every book to be queued may be contributed (the Bridge can): the contribute choice is offered. */
  canContribute: boolean;
}

export function batchPlan<B extends { library_id: number; path: string }>(
  ordered: readonly B[],
  checks: ReadonlyMap<string, BatchCheck>,
): BatchPlan<B> {
  const plan: BatchPlan<B> = {
    queue: [],
    covered: 0,
    open: 0,
    unknown: 0,
    notWritable: false,
    costUsd: 0,
    subscription: false,
    canContribute: true,
  };
  for (const b of ordered) {
    const c = checks.get(refKey(b));
    if (c?.covered) plan.covered++;
    else if (c?.open) plan.open++;
    else if (!c || c.unknown) plan.unknown++;
    else {
      // Only a book that would be queued can stop the batch.
      if (c.notWritable) plan.notWritable = true;
      plan.costUsd = plan.costUsd === null || c.costUsd === null ? null : plan.costUsd + c.costUsd;
      plan.subscription ||= c.subscription;
      plan.canContribute &&= c.canContribute;
      plan.queue.push(b);
    }
  }
  if (!plan.queue.length) {
    plan.costUsd = null;
    plan.canContribute = false;
  }
  return plan;
}

/**
 * What the book dialog's plan costs in all: this book (its estimate) and the
 * earlier books it queues first (each costed as audio from its length). A total
 * is null when any book's figure is unknown; `books` counts the runs.
 */
export function planEstimate(
  e: BridgeEstimate,
  includeEarlier: boolean,
): { books: number; asrHours: number | null; costUsd: number | null } {
  const v = estimateView(e);
  let { asrHours, costUsd } = v;
  const earlier = includeEarlier ? e.earlier : [];
  for (const b of earlier) {
    // An earlier book is costed as audio: no transcription figure is no estimate.
    const asr = amount(b.asr_hours);
    asrHours = asrHours === null || !asr ? null : asrHours + asr;
    const cost = amount(b.agent_cost_usd);
    costUsd = costUsd === null || cost === undefined ? null : costUsd + cost;
  }
  return { books: earlier.length + 1, asrHours, costUsd };
}

/**
 * Runs `fn` over the items at most `limit` at a time, resolving every outcome
 * in the items' order (a failure is undefined, never a rejection), and calls
 * `onEach` as each settles. The series batch checks its books this way, so a
 * long series doesn't send every estimate at once.
 */
export async function settleLimited<T, R>(
  items: readonly T[],
  limit: number,
  fn: (item: T) => Promise<R>,
  onEach?: (settled: number) => void,
  signal?: AbortSignal,
): Promise<(R | undefined)[]> {
  const out: (R | undefined)[] = new Array<R | undefined>(items.length).fill(undefined);
  let next = 0;
  let settled = 0;
  const worker = async () => {
    while (next < items.length && !signal?.aborted) {
      const i = next++;
      try {
        out[i] = await fn(items[i]);
      } catch {
        out[i] = undefined;
      }
      onEach?.(++settled);
    }
  };
  await Promise.all(Array.from({ length: Math.max(1, Math.min(limit, items.length)) }, worker));
  return out;
}
