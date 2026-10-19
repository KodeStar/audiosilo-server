import type {
  AdminBook,
  BridgeEstimate,
  BridgeJob,
  BridgeJobRequest,
  BridgeStatus,
  ServerInfo,
  SidecarsContributeParams,
  SidecarsCoverage,
  SidecarsGenerateParams,
} from '@/api/types';
import { refKey } from '@/lib/book-route';
import { relBaseName } from '@/lib/paths';
import { spanIn } from '@/features/library/series/series-model';
import { isOpen, mappingState, num, progressOf, safeHttpUrl, text } from './bridge-model';

// Characters and recaps from the Bridge (BRIDGE-PLAN.md Phase 1a), the
// description written from their notes (Phase 1c), the chapters found in the
// narration (Phase 1d) and sharing them and corrections with the community
// (Phase 2b): the rules the book page's Community card, its generate dialog,
// the series batch, Health's Detect chapters and the Bridge page's job rows
// follow for `sidecars.generate`, `sidecars.describe`, `chapters.detect`,
// `sidecars.contribute` and `meta.correct`. Pure: no React, no i18n instance;
// callers pass the keys these return to t().

/** The job kind, and the feature (job family) the Bridge advertises for it. */
export const KIND_GENERATE = 'sidecars.generate';
export const FEATURE_SIDECARS = 'sidecars';
/** Phase 1c's description-only kind: a description from the notes the Bridge kept. */
export const KIND_DESCRIBE = 'sidecars.describe';
/**
 * Phase 1d's chapter detection for a book with no chapter markers (speech
 * recognition, no agent), and its feature (job family).
 */
export const KIND_DETECT = 'chapters.detect';
export const FEATURE_CHAPTERS = 'chapters';
/**
 * Phase 2b's "Share with the community": the community issues filed from the
 * characters and recaps already generated for a book (sidecars family).
 */
export const KIND_CONTRIBUTE = 'sidecars.contribute';
/**
 * Phase 2b's corrections: an admin's edited fields filed as one correction-batch
 * issue, and the feature (job family) the Bridge advertises for it.
 */
export const KIND_CORRECT = 'meta.correct';
export const FEATURE_META = 'meta';

/**
 * The kinds the book page follows: generate, describe and share (the sidecars
 * family; the Community card's sections), corrections (its corrections part) and
 * chapter detection, whose end changes the book's chapters (the Chapters card
 * follows it).
 */
const BOOK_KINDS: readonly string[] = [
  KIND_GENERATE,
  KIND_DESCRIBE,
  KIND_DETECT,
  KIND_CONTRIBUTE,
  KIND_CORRECT,
];

/** The kinds that hold the book's generated files on the Bridge (a describe job waits for them). */
const FILE_KINDS: readonly string[] = [KIND_GENERATE, KIND_DESCRIBE, KIND_DETECT];

/**
 * Whether a kind is one the book page follows; its progress and result are
 * worded by GenerateStatus, on the book page and the Bridge page's job rows.
 */
export function isBookKind(kind: string): boolean {
  return BOOK_KINDS.includes(kind);
}

/** The book's jobs of these kinds (the book page's, by default) that are still open. */
function openBookJobs<J extends Pick<BridgeJob, 'kind' | 'state'>>(
  jobs: readonly J[] | undefined,
  kinds: readonly string[] = BOOK_KINDS,
): J[] {
  return jobs?.filter((j) => kinds.includes(j.kind) && isOpen(j.state)) ?? [];
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
 * Whether the contributions module is on (Settings > Modules, and an online
 * Bridge that can contribute: the capability is both). Off, every Contribute,
 * Share and Suggest corrections control is gone.
 */
export function contributionsOn(server: ServerInfo | undefined): boolean {
  return Boolean(server?.capabilities.contributions);
}

/**
 * Whether a generate dialog offers the contribute switch: the module is on and
 * the Bridge can file contributions now (`canContribute`: the estimate's, which
 * the server reads from the Bridge's checklist, else its older figures). The
 * switch then starts on: a signed-in Bridge contributes unless told not to.
 */
export function contributeOffered(server: ServerInfo | undefined, canContribute: boolean): boolean {
  return contributionsOn(server) && canContribute;
}

/**
 * Whether "Detect chapters" is offered (Health's books without chapters): an
 * online Bridge offers the chapters family (chapters.detect). It needs no
 * community metadata: the chapters are the book's own.
 */
export function detectAvailable(server: ServerInfo | undefined): boolean {
  if (!server?.capabilities.bridge) return false;
  return server.bridge?.features.includes(FEATURE_CHAPTERS) ?? false;
}

/** What the Detect chapters confirmation says about the books it queues. */
export interface DetectPlan {
  /**
   * The transcription time in hours, all books together; null without the
   * Bridge's figures (or a figure of 0), any known length, or a total that
   * rounds to nothing.
   */
  asrHours: number | null;
  /** A book's library the Bridge can't write to: the server refuses its job. */
  notWritable: boolean;
}

/**
 * The Detect chapters confirmation's figures, from the Bridge's status (its
 * heartbeat's ASR figure and library mappings): the transcription time over
 * the books' known lengths, rounded per book to hundredths of an hour as the
 * server's generate estimate rounds asr_hours (and, as estimateView reads that,
 * no time at all is no estimate), and whether any book's library can't be
 * written to.
 */
export function detectPlan(
  status: Pick<BridgeStatus, 'libraries' | 'estimates'> | undefined,
  books: readonly Pick<AdminBook, 'library_id' | 'duration'>[],
): DetectPlan {
  const perHour = status?.estimates?.sidecars?.asr_seconds_per_audio_hour ?? 0;
  const total = books
    .filter((b) => b.duration > 0)
    .reduce((sum, b) => sum + Math.round(((b.duration / 3600) * perHour) / 36) / 100, 0);
  const writable = (id: number) =>
    mappingState(status?.libraries.find((l) => l.id === id)?.mapping ?? null) === 'ready';
  return {
    asrHours: total > 0 ? total : null,
    notWritable: status ? books.some((b) => !writable(b.library_id)) : false,
  };
}

/**
 * Whether the book page should ask for its generate and describe jobs at all: a
 * server that knows the Bridge (it reports the capability, on or off) with
 * metadata on. A job queued while the Bridge was online stays worth showing
 * while it's away.
 */
export function generateJobsKnown(server: ServerInfo | undefined): boolean {
  return Boolean(server?.capabilities.metadata && server.capabilities.bridge !== undefined);
}

/**
 * Whether the book page should follow its chapter detection jobs: a server
 * that knows the Bridge (on or off). Detection needs no community metadata, so
 * unlike generateJobsKnown this doesn't ask for it.
 */
export function detectJobsKnown(server: ServerInfo | undefined): boolean {
  return server?.capabilities.bridge !== undefined;
}

/**
 * What the card offers (BRIDGE-PLAN.md Phase 1e, the one offer rule the player
 * follows too): `covered` when the community has both characters and recaps
 * (no Generate; a line says so and links to the Characters and recaps card,
 * even over files generated here before, which the community's masks), else
 * Regenerate while either dimension was generated here, else Generate. Generated
 * content only ever fills a dimension the community lacks (/meta's per-dimension
 * merge), so Generate stays offered while either is missing. `community` is the
 * community's own coverage (the estimate's `has.community`), never generated.
 */
export type GenerateAction = 'generate' | 'regenerate' | 'covered';

export function generateAction(has: {
  community: SidecarsCoverage;
  local: SidecarsCoverage;
}): GenerateAction {
  if (has.community.characters && has.community.recaps) return 'covered';
  return has.local.characters || has.local.recaps ? 'regenerate' : 'generate';
}

/**
 * The community issues the book's newest finished generate or share job filed,
 * by what each offers (`characters`, `recaps`): the Characters and recaps
 * card's "Contributed, waiting for review" links, and what Share no longer
 * offers. An open or failed job since doesn't hide them (they still wait), a
 * newer finished one replaces them (a run kept on this server made new content,
 * not yet offered). Whether a contribution was reviewed doesn't reach the
 * server yet (decision D8).
 */
export function contributedLinks(
  jobs: readonly Pick<BridgeJob, 'kind' | 'state' | 'result'>[] | undefined,
): ContributedLinks {
  const done = jobs?.find(
    (j) => (j.kind === KIND_GENERATE || j.kind === KIND_CONTRIBUTE) && j.state === 'done',
  );
  const out: ContributedLinks = {};
  for (const c of generateResult(done?.result)?.contributed ?? []) {
    if (c.what === 'characters' || c.what === 'recaps') out[c.what] = c.url;
  }
  return out;
}

/** A generate job's community issues for the two dimensions it offers. */
export type ContributedLinks = { characters?: string; recaps?: string };

/** The two things a book's generated files offer the community. */
export type ShareDimension = 'characters' | 'recaps';

/**
 * What "Share with the community" would offer: each dimension generated here
 * that the community lacks and no finished job has filed yet.
 */
export function shareDimensions(
  has: { community: SidecarsCoverage; local: SidecarsCoverage },
  links: ContributedLinks,
): ShareDimension[] {
  return (['characters', 'recaps'] as const).filter(
    (d) => has.local[d] && !has.community[d] && !links[d],
  );
}

/**
 * Whether the Community card offers "Share with the community" (Phase 2b's
 * contribute later), and for what: the module is on, an online Bridge offers
 * the sidecars family and can contribute now, the community answered (unknown
 * is never "lacks"), no generate or share job is open for the book (the server
 * refuses one then), and something generated here is still to offer. Empty
 * when not offered.
 */
export function shareOffered(
  server: ServerInfo | undefined,
  e: Pick<BridgeEstimate, 'can_contribute' | 'has' | 'open_job'> | undefined,
  jobs: readonly Pick<BridgeJob, 'kind' | 'state' | 'result'>[] | undefined,
): ShareDimension[] {
  if (!contributionsOn(server) || !generateAvailable(server)) return [];
  if (!e?.can_contribute || e.has.community_unknown) return [];
  // The estimate's open job: a generate run (it may contribute itself) or a
  // share already queued (its status shows; never a second Share).
  const oj = e.open_job;
  if (oj && (oj.kind === KIND_GENERATE || oj.kind === KIND_CONTRIBUTE) && isOpen(oj.state))
    return [];
  if (openBookJobs(jobs, [KIND_GENERATE, KIND_CONTRIBUTE]).length) return [];
  return shareDimensions(e.has, contributedLinks(jobs));
}

/** The share job for a book: only the dimensions still to offer (the Bridge skips what the community has). */
export function shareRequest(
  book: { library_id: number; path: string },
  dimensions: readonly ShareDimension[],
): BridgeJobRequest {
  const params: SidecarsContributeParams = { dimensions: [...dimensions] };
  return {
    kind: KIND_CONTRIBUTE,
    library_id: book.library_id,
    path: book.path,
    params: { ...params },
  };
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

/**
 * Whether a job keeps a new generate job from being queued for its book: a
 * chapter detection (the server's detect_open) or a share with the community
 * (contribute_open, Phase 2b) queued or working, not being cancelled. A parked
 * one gives way (the generate job cancels it: a detection's chapters it finds
 * itself, a share it offers again from the new run), as one being cancelled does.
 */
export function blocksGenerate(j: Pick<BridgeJob, 'kind' | 'state' | 'cancel_requested'>): boolean {
  return (
    (j.kind === KIND_DETECT || j.kind === KIND_CONTRIBUTE) &&
    (j.state === 'queued' || j.state === 'claimed' || j.state === 'running') &&
    !j.cancel_requested
  );
}

/**
 * The book's job that keeps Generate from being offered now (blocksGenerate):
 * from its job list once loaded (the live answer), else the estimate's
 * `open_job`, which names such a job when no generate job is open.
 */
export function generateBlocker(
  jobs: readonly BridgeJob[] | undefined,
  openJob: BridgeJob | undefined,
): BridgeJob | undefined {
  if (!jobs) return openJob && blocksGenerate(openJob) ? openJob : undefined;
  return [latestJobOf(jobs, KIND_DETECT), latestJobOf(jobs, KIND_CONTRIBUTE)].find(
    (j): j is BridgeJob => j !== undefined && blocksGenerate(j),
  );
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
 * sidecars.describe) and can write to the library (the server refuses a job
 * otherwise), the book has characters or recaps generated here (their notes are
 * what the description is written from), it has no description yet, here or
 * from the community (which must have answered: unknown is never "none"), and
 * no generate, describe or chapter detection job is open for it (a generate job
 * writes one when it finishes; a detection, running or parked, holds the book
 * on the Bridge, which fails a describe meanwhile; the estimate's own open
 * generate job counts too, so a job list not loaded yet can't offer what the
 * server would refuse).
 */
export function describeOffered(
  e: Pick<BridgeEstimate, 'can_describe' | 'writable' | 'has' | 'open_job'> | undefined,
  jobs: readonly Pick<BridgeJob, 'kind' | 'state'>[] | undefined,
): boolean {
  if (!e?.can_describe || !e.writable || e.open_job) return false;
  const { local, community, community_unknown } = e.has;
  if (
    !(local.characters || local.recaps) ||
    local.description ||
    community.description ||
    community_unknown
  ) {
    return false;
  }
  return openBookJobs(jobs, FILE_KINDS).length === 0;
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
  'chapter_detecting',
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
  'chapters_not_detected',
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
  // The Bridge's own last steps (the description, saving the files, the
  // rescan) say what is happening; the engine's stage by then is only where it
  // stopped ("done", or "contributing").
  const stage =
    (step === 'describing' || step === 'writing' || step === 'rescanning' ? step : undefined) ??
    text(p.stage) ??
    step;
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
  /** A chapters.detect job's chapters found. */
  chapters?: number;
  costUsd?: number;
  /** The description was written (true) or not (false, with why); absent from an older Bridge. */
  description?: boolean;
  descriptionError?: string;
  /** The community issues filed (http(s) links only), by what each offers. */
  contributed: { what: string; url: string }[];
  /** A share job's dimensions the community already had (nothing filed for them). */
  covered?: string[];
  /** A corrections job's issue (an http(s) link) and its number. */
  issue?: { url: string; number?: number };
  /** A corrections job's change count, and its fields by who applies them. */
  changes?: number;
  auto?: string[];
  maintainer?: string[];
}

/** A list of non-blank strings from an opaque result, else undefined. */
function strings(v: unknown): string[] | undefined {
  if (!Array.isArray(v)) return undefined;
  const out = v.map(text).filter((s): s is string => s !== undefined);
  return out;
}

/** Reads a sidecars.generate (or chapters.detect) result leniently, or null when it holds none of its facts. */
export function generateResult(result: unknown): GenerateResult | null {
  if (!result || typeof result !== 'object') return null;
  const r = result as Record<string, unknown>;
  const out: GenerateResult = { contributed: [] };
  const characters = amount(r.characters);
  if (characters !== undefined) out.characters = characters;
  const recaps = amount(r.recaps);
  if (recaps !== undefined) out.recaps = recaps;
  const chapters = amount(r.chapters);
  if (chapters !== undefined) out.chapters = chapters;
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
  const covered = strings(r.covered);
  if (covered?.length) out.covered = covered;
  const issueUrl = typeof r.issue_url === 'string' ? safeHttpUrl(r.issue_url) : undefined;
  if (issueUrl) {
    out.issue = { url: issueUrl };
    const n = amount(r.issue_number);
    if (n) out.issue.number = n;
  }
  // The change count, or the changes themselves.
  const changes = Array.isArray(r.changes) ? r.changes.length : amount(r.changes);
  if (changes !== undefined) out.changes = changes;
  const auto = strings(r.auto);
  if (auto) out.auto = auto;
  const maintainer = strings(r.maintainer);
  if (maintainer) out.maintainer = maintainer;
  const any =
    out.characters !== undefined ||
    out.recaps !== undefined ||
    out.chapters !== undefined ||
    out.costUsd !== undefined ||
    out.description !== undefined ||
    out.contributed.length > 0 ||
    out.covered !== undefined ||
    out.issue !== undefined ||
    out.changes !== undefined;
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
  /**
   * A generate job is already open for it (and not being cancelled), or its
   * chapters are being found (the estimate's `open_job` names that job): skipped.
   */
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
