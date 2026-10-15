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
import { sameSeries } from '@/lib/series-key';
import { isOpen, safeHttpUrl } from './bridge-model';

// Characters and recaps from the Bridge (BRIDGE-PLAN.md Phase 1a): the rules the
// book page's Community card, its generate dialog, the series batch and the
// Bridge page's job rows follow for `sidecars.generate`. Pure: no React, no i18n
// instance; callers pass the keys these return to t().

/** The job kind, and the feature (job family) the Bridge advertises for it. */
export const KIND_GENERATE = 'sidecars.generate';
export const FEATURE_SIDECARS = 'sidecars';

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

/** How often a book's generate jobs are asked again: about every 10 s while one is open, else not. */
export const GENERATE_POLL_MS = 10_000;

export function generatePollMs(
  jobs: readonly Pick<BridgeJob, 'kind' | 'state'>[] | undefined,
): number | false {
  return jobs?.some((j) => j.kind === KIND_GENERATE && isOpen(j.state)) ? GENERATE_POLL_MS : false;
}

/** The book's newest generate job (the list is newest first), if any. */
export function latestGenerateJob<J extends Pick<BridgeJob, 'kind'>>(
  jobs: readonly J[] | undefined,
): J | undefined {
  return jobs?.find((j) => j.kind === KIND_GENERATE);
}

/**
 * Whether a job just finished: it was open the last time it was seen and is
 * done now. The card then refetches the book, so the new local data shows.
 */
export function justFinished(
  before: Pick<BridgeJob, 'id' | 'state'> | undefined,
  now: Pick<BridgeJob, 'id' | 'state'> | undefined,
): boolean {
  return Boolean(
    before && now && before.id === now.id && isOpen(before.state) && now.state === 'done',
  );
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

const num = (v: unknown): number | undefined =>
  typeof v === 'number' && Number.isFinite(v) && v >= 0 ? v : undefined;
const text = (v: unknown): string | undefined =>
  typeof v === 'string' && v.trim() ? v.trim() : undefined;
const sourceOf = (v: unknown): 'audio' | 'ebook' | undefined =>
  v === 'audio' || v === 'ebook' ? v : undefined;

/** A generate job's progress as the card words it; every field only when the Bridge sent it. */
export interface GenerateProgress {
  /** The engine stage, else the step. */
  stage?: string;
  message?: string;
  done?: number;
  total?: number;
  etaSeconds?: number;
  costUsd?: number;
  source?: 'audio' | 'ebook';
  park?: { code: string; hint?: string; retryAt?: string };
}

/** Reads a sidecars.generate progress leniently (the server stores it opaquely). */
export function generateProgress(progress: unknown): GenerateProgress {
  if (!progress || typeof progress !== 'object') return {};
  const p = progress as Record<string, unknown>;
  const out: GenerateProgress = {};
  const stage = text(p.stage) ?? text(p.step);
  if (stage) out.stage = stage;
  const message = text(p.message);
  if (message) out.message = message;
  const done = num(p.done);
  const total = num(p.total);
  if (done !== undefined && total !== undefined && total > 0) {
    out.done = done;
    out.total = total;
  }
  const eta = num(p.eta_seconds);
  if (eta !== undefined) out.etaSeconds = eta;
  const cost = num(p.cost_usd);
  if (cost !== undefined) out.costUsd = cost;
  const source = sourceOf(p.source);
  if (source) out.source = source;
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

/** A finished generate job's result as the card words it. */
export interface GenerateResult {
  characters?: number;
  recaps?: number;
  costUsd?: number;
  source?: 'audio' | 'ebook';
  /** The community issues filed (http(s) links only), by what each offers. */
  contributed: { what: string; url: string }[];
}

/** Reads a sidecars.generate result leniently, or null when it holds none of its facts. */
export function generateResult(result: unknown): GenerateResult | null {
  if (!result || typeof result !== 'object') return null;
  const r = result as Record<string, unknown>;
  const out: GenerateResult = { contributed: [] };
  const characters = num(r.characters);
  if (characters !== undefined) out.characters = characters;
  const recaps = num(r.recaps);
  if (recaps !== undefined) out.recaps = recaps;
  const cost = num(r.cost_usd);
  if (cost !== undefined) out.costUsd = cost;
  const source = sourceOf(r.source);
  if (source) out.source = source;
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

/** The last segment of a library path ("Author/Book/Book.epub" -> "Book.epub"). */
export function baseName(path: string): string {
  const parts = path.split('/').filter(Boolean);
  return parts[parts.length - 1] ?? path;
}

/**
 * The confirmation dialog's estimate, as facts the dialog words: the source
 * (and the ebook's file), the transcription it needs, the agent cost the
 * Bridge's history predicts, and what stops or warns.
 */
export interface EstimateView {
  source: 'audio' | 'ebook';
  ebook?: { name: string; path: string };
  /** Transcription hours; 0 = none needed (an ebook); null = no estimate yet. */
  asrHours: number | null;
  /** The expected agent cost; null = no estimate yet. */
  costUsd: number | null;
  /** The Bridge's agent runs on a subscription: the cost is an estimate of usage. */
  subscription: boolean;
  /** The agent isn't ready: the job parks until it is. */
  agentNotReady: boolean;
  /** The Bridge can't write to the library: nothing can be queued. */
  notWritable: boolean;
  canConfirm: boolean;
}

export function estimateView(e: BridgeEstimate): EstimateView {
  const ebook = e.source === 'ebook';
  const asr = num(e.asr_hours);
  return {
    source: e.source,
    ...(ebook && e.ebook ? { ebook: { name: baseName(e.ebook), path: e.ebook } } : {}),
    asrHours: ebook ? 0 : asr && asr > 0 ? asr : null,
    costUsd: num(e.agent_cost_usd) ?? null,
    subscription: e.subscription,
    agentNotReady: !e.agent_ready,
    notWritable: !e.writable,
    canConfirm: e.writable,
  };
}

/** Where the results appear, by the contribute choice (decision 6: local by default). */
export function destinationKey(contribute: 'local' | 'issue'): string {
  return contribute === 'issue' ? 'bridge.generate.whereIssue' : 'bridge.generate.whereLocal';
}

/**
 * The jobs the dialog queues, in order: the earlier books of the series first
 * (in reading order, when asked to), then this book. Ids ascend, so the Bridge
 * claims them in that order.
 */
export function generatePlan(
  book: { library_id: number; path: string },
  earlier: readonly { library_id: number; path: string }[],
  opts: { includeEarlier: boolean; contribute: 'local' | 'issue' },
): BridgeJobRequest[] {
  const params: SidecarsGenerateParams = { source: 'auto', contribute: opts.contribute };
  const books = [...(opts.includeEarlier ? earlier : []), book];
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
export function seriesBatchOrder<B extends Pick<AdminBook, 'library_id' | 'path' | 'series_list'>>(
  books: readonly B[],
  series: string,
): B[] {
  const seen = new Set<string>();
  const unique = books.filter((b) => {
    const key = refKey(b);
    if (seen.has(key)) return false;
    seen.add(key);
    return true;
  });
  // Its position in this series (series_list holds one per series, the way
  // series-model's spanIn reads it); 0 when it has none.
  const pos = (b: B) => b.series_list.find((r) => sameSeries(r.name, series))?.position ?? 0;
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
  /** A generate job is already open for it: skipped (the server would return it). */
  open: boolean;
  /** The Bridge can't write to its library. */
  notWritable: boolean;
}

export function batchCheck(e: BridgeEstimate | undefined): BatchCheck {
  // A book whose estimate failed is queued anyway: the server dedupes open jobs.
  if (!e) return { covered: false, open: false, notWritable: false };
  return {
    covered: e.has.community.recaps || e.has.local.recaps,
    open: Boolean(e.open_job && isOpen(e.open_job.state)),
    notWritable: !e.writable,
  };
}

/** The series batch's split: the books to queue (in order), and how many were skipped and why. */
export function batchPlan<B extends { library_id: number; path: string }>(
  ordered: readonly B[],
  checks: ReadonlyMap<string, BatchCheck>,
): { queue: B[]; covered: number; open: number; notWritable: boolean } {
  const queue: B[] = [];
  let covered = 0;
  let open = 0;
  let notWritable = false;
  for (const b of ordered) {
    const c = checks.get(refKey(b));
    if (c?.notWritable) notWritable = true;
    if (c?.covered) covered++;
    else if (c?.open) open++;
    else queue.push(b);
  }
  return { queue, covered, open, notWritable };
}

/** A generate job's line on the card: what state it's in, as the card shows it. */
export type GenerateLook = 'working' | 'waiting' | 'parked' | 'done' | 'failed' | 'cancelled';

export function generateLook(job: Pick<BridgeJob, 'state'>): GenerateLook {
  switch (job.state) {
    case 'queued':
      return 'waiting';
    case 'needs_attention':
      return 'parked';
    case 'done':
      return 'done';
    case 'failed':
      return 'failed';
    case 'cancelled':
      return 'cancelled';
    default:
      return 'working';
  }
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
