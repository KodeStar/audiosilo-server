import type { ComponentProps } from 'react';
import type { BridgeJob, BridgeJobState, BridgeMapping, BridgeStatus } from '@/api/types';
import type { StatusTone } from '@/components/status-text';
import type { Badge } from '@/components/ui/badge';
import { isLoopbackUrl } from '@/features/setup/setup-model';
import { absoluteBaseName } from '@/lib/paths';

// Settings > Bridge: the small rules the page shows the Bridge and its jobs by
// (internal/bridge, handlers_bridge.go). Pure: no React, no i18n instance; the
// page passes the keys these return to t(), or passes t in.

/** i18next's t as far as these rules use it. */
type Translate = (key: string, opts?: Record<string, unknown>) => string;

/** Every job state, in the order the filter lists them. */
export const BRIDGE_JOB_STATES = [
  'queued',
  'claimed',
  'running',
  'needs_attention',
  'done',
  'failed',
  'cancelled',
] as const satisfies readonly BridgeJobState[];

/** Phase 0's proof job: the Bridge checks a library (or a book) through its mapping. */
export const KIND_VERIFY = 'bridge.verify';

/** The longest hold bridge.verify takes (bridge.verifyParams). */
export const MAX_HOLD_SECONDS = 600;

/** A job kind's name (`bridge.kind.<kind>`), or its code when this console has none. */
export function kindName(t: Translate, kind: string): string {
  return t(`bridge.kind.${kind}`, { defaultValue: kind });
}

/** A state the Bridge can still move a job on from (bridge.IsOpen). */
export function isOpen(state: BridgeJobState): boolean {
  return (
    state === 'queued' || state === 'claimed' || state === 'running' || state === 'needs_attention'
  );
}

/** What can be done to a job, and whether a cancel is waiting on the Bridge. */
export function jobActions(job: Pick<BridgeJob, 'state' | 'cancel_requested'>): {
  cancel: boolean;
  cancelling: boolean;
} {
  // A claimed or running job stops at the Bridge's next report; until then it
  // is "cancelling" and asking again would change nothing.
  const cancelling = job.cancel_requested && (job.state === 'claimed' || job.state === 'running');
  return { cancel: isOpen(job.state) && !cancelling, cancelling };
}

type BadgeTone = ComponentProps<typeof Badge>['variant'];

/** A job's state badge: its i18n key and tone ("cancelling" over a running state). */
export function stateLook(job: Pick<BridgeJob, 'state' | 'cancel_requested'>): {
  key: string;
  tone: BadgeTone;
} {
  if (jobActions(job).cancelling) return { key: 'bridge.state.cancelling', tone: 'warning' };
  const tones: Record<BridgeJobState, BadgeTone> = {
    queued: 'secondary',
    claimed: 'info',
    running: 'info',
    needs_attention: 'warning',
    done: 'success',
    failed: 'destructive',
    cancelled: 'outline',
  };
  return { key: `bridge.state.${job.state}`, tone: tones[job.state] ?? 'secondary' };
}

/** How often the job list asks again: every few seconds while any job is open. */
export const JOBS_POLL_OPEN_MS = 3000;
export const JOBS_POLL_IDLE_MS = 30_000;

export function jobsPollMs(jobs: readonly Pick<BridgeJob, 'state'>[]): number {
  return jobs.some((j) => isOpen(j.state)) ? JOBS_POLL_OPEN_MS : JOBS_POLL_IDLE_MS;
}

/** How often the status asks again: every few seconds while a pairing code waits. */
export const STATUS_POLL_PAIRING_MS = 3000;
export const STATUS_POLL_MS = 15_000;

export function statusPollMs(status: Pick<BridgeStatus, 'pairing'> | undefined): number {
  return status?.pairing ? STATUS_POLL_PAIRING_MS : STATUS_POLL_MS;
}

/**
 * Whether a Bridge took the code this page made: paired now, and paired at
 * another moment than when the code was made (an expired code leaves the old
 * pairing as it was; a new Bridge's exchange stamps a new one).
 */
export function codeExchanged(
  pairedAtBefore: string | undefined,
  status: Pick<BridgeStatus, 'paired' | 'paired_at'> | undefined,
): boolean {
  return Boolean(status?.paired && status.paired_at && status.paired_at !== pairedAtBefore);
}

/** Whole seconds left until `expiresAt` (0 once past or unreadable). */
export function secondsLeft(expiresAt: string, now: number): number {
  const at = Date.parse(expiresAt);
  if (Number.isNaN(at)) return 0;
  return Math.max(0, Math.ceil((at - now) / 1000));
}

/** Whether the paired Bridge is heard from now, has gone quiet, or hasn't reported yet. */
type Presence = 'online' | 'offline' | 'waiting';

export function presence(status: Pick<BridgeStatus, 'online' | 'heartbeat_at'>): Presence {
  if (status.online) return 'online';
  return status.heartbeat_at ? 'offline' : 'waiting';
}

export const PRESENCE_TONE: Record<Presence, StatusTone> = {
  online: 'ok',
  offline: 'bad',
  waiting: 'off',
};

/** How a library's mapping reads at a glance. */
type MappingState = 'ready' | 'readOnly' | 'notMapped' | 'unknown';

export function mappingState(m: BridgeMapping | null): MappingState {
  if (!m) return 'unknown';
  if (!m.mapped) return 'notMapped';
  return m.writable ? 'ready' : 'readOnly';
}

export const MAPPING_TONE: Record<MappingState, StatusTone> = {
  ready: 'ok',
  readOnly: 'warn',
  notMapped: 'bad',
  unknown: 'off',
};

/**
 * A link to the Bridge's own page, only for an absolute http(s) URL (the server
 * checks the same; a page link is never a javascript: or data: URL).
 */
export function safeHttpUrl(url: string | undefined): string | undefined {
  if (!url) return undefined;
  try {
    const u = new URL(url);
    return (u.protocol === 'http:' || u.protocol === 'https:') && u.host ? u.href : undefined;
  } catch {
    return undefined;
  }
}

/** A tab of the Bridge's page (`<ui_url>#/<tab>`), only for an http(s) ui_url. */
export function bridgeTabLink(uiUrl: string | undefined, tab: string): string | undefined {
  const base = safeHttpUrl(uiUrl);
  if (!base) return undefined;
  const u = new URL(base);
  u.hash = `/${tab}`;
  return u.href;
}

/** A finite number from an opaque progress or result, else undefined. */
export const num = (v: unknown): number | undefined =>
  typeof v === 'number' && Number.isFinite(v) ? v : undefined;
/** A non-blank string from an opaque progress or result (trimmed), else undefined. */
export const text = (v: unknown): string | undefined =>
  typeof v === 'string' && v.trim() ? v.trim() : undefined;

/**
 * What a job's progress says, leniently: its message (else its step) and a
 * done/total count when both are numbers. Any kind's progress reads this way.
 */
export function progressOf(progress: unknown): { text?: string; done?: number; total?: number } {
  if (!progress || typeof progress !== 'object') return {};
  const p = progress as Record<string, unknown>;
  const done = num(p.done);
  const total = num(p.total);
  return {
    text: text(p.message) ?? text(p.step),
    ...(done !== undefined && total !== undefined && total > 0 ? { done, total } : {}),
  };
}

/** A result's facts the page words (bridge.verify's), each only when present. */
interface ResultSummary {
  /** Files checked, and how many matched the size the server recorded. */
  files?: { checked: number; ok: number };
  writable?: boolean;
  elapsedMs?: number;
  /** Where the Bridge looked. */
  mountPath?: string;
}

/**
 * A compact summary of a job's result, or null when it holds none of the facts
 * this page words (a later kind's result is still in the details).
 */
export function resultSummary(result: unknown): ResultSummary | null {
  if (!result || typeof result !== 'object') return null;
  const r = result as Record<string, unknown>;
  const out: ResultSummary = {};
  if (Array.isArray(r.files)) {
    const files = r.files as unknown[];
    out.files = {
      checked: files.length,
      ok: files.filter((f) => f && typeof f === 'object' && (f as { ok?: unknown }).ok === true)
        .length,
    };
  }
  if (typeof r.writable === 'boolean') out.writable = r.writable;
  const ms = num(r.elapsed_ms);
  if (ms !== undefined) out.elapsedMs = ms;
  const mount = text(r.mount_path);
  if (mount) out.mountPath = mount;
  return Object.keys(out).length ? out : null;
}

/** Whether an object carries anything (a job's result or progress is `{}` when empty). */
export function hasContent(v: unknown): boolean {
  return Boolean(v && typeof v === 'object' && Object.keys(v).length);
}

/**
 * The hold the verify dialog's field asks for, as bridge.verify's params: a
 * whole number of seconds 0-600, or null when the text isn't one. 0 sends no
 * params at all.
 */
export function holdParams(textValue: string): { hold_seconds: number } | undefined | null {
  const s = textValue.trim();
  if (s === '') return undefined;
  const n = Number(s);
  if (!Number.isInteger(n) || n < 0 || n > MAX_HOLD_SECONDS) return null;
  return n === 0 ? undefined : { hold_seconds: n };
}

// --- Adding a Bridge ------------------------------------------------------------

/** "Running a Bridge" in the User Guide: the compose file, the agent container and the checklist. */
export const RUNNING_A_BRIDGE_DOCS_URL =
  'https://docs.audiosilo.app/users/getting-started/running-a-bridge';

/**
 * The server as a Bridge in the same compose file reaches it: its service name
 * (audiosilo-bridge's compose.example.yaml, and the docs' compose file).
 */
export const COMPOSE_SERVER_URL = 'http://audiosilo:8080';

/**
 * The server address to give a Bridge: the server's own suggestion (its home
 * address, else the address this page uses), unless that is a loopback one.
 * From inside a Bridge's container 127.0.0.1 is the Bridge itself, so the
 * compose service name is offered then (`loopback`), the one the compose
 * snippet uses.
 */
export function bridgeServerUrl(suggested: string): { url: string; loopback: boolean } {
  return isLoopbackUrl(suggested)
    ? { url: COMPOSE_SERVER_URL, loopback: true }
    : { url: suggested, loopback: false };
}

/**
 * The Bridge's service for the server's compose file, from audiosilo-bridge's
 * compose.example.yaml. It mounts every library folder read-write at the
 * server's own path (`roots`, one volume each), so the Bridge finds each
 * library with no mapping; with no library yet, an example volume says so.
 * The agent container (characters and recaps) is in Running a Bridge.
 */
export function bridgeCompose(roots: readonly string[]): string {
  const at = [...new Set(roots.map((r) => r.trim()).filter(Boolean))];
  const volumes = (at.length ? at : ['/audiobooks']).map(
    (root) => `      - /path/to/${hostFolder(root)}:${root}:rw`,
  );
  // With no library yet there's no server path to give: say the line is an example.
  const volumesNote = at.length
    ? '# Each library folder, at the same path the server uses:'
    : "# Your books folder, at the path the server will use (no library yet, so this one's an example):";
  return `  bridge:
    image: ghcr.io/kodestar/audiosilo-bridge:latest
    restart: unless-stopped
    depends_on: [audiosilo]
    environment:
      PUID: "1000"
      PGID: "1000"
      # The server as the Bridge reaches it on this network (its service name).
      AUDIOSILO_BRIDGE_SERVER_URL: ${COMPOSE_SERVER_URL}
      # The Bridge's page as your LAN reaches it (the console links to it).
      AUDIOSILO_BRIDGE_PUBLIC_URL: http://192.168.1.10:8091
      AUDIOSILO_BRIDGE_AGENT_STAGING_DIR: /staging
    ports:
      - "8091:8091"
    volumes:
      - ./bridge-data:/bridge-data
      ${volumesNote}
${volumes.join('\n')}
      - staging:/staging

# And under the file's top-level volumes:
#   staging: {}`;
}

/** A placeholder host folder for a library root: its last segment ("/mnt/kids" -> "kids"). */
function hostFolder(root: string): string {
  return absoluteBaseName(root).replace(/[^A-Za-z0-9._-]/g, '-') || 'audiobooks';
}

/**
 * Whether a job of this kind is one the server queues itself (a podcast's feed
 * checks, episode fetches and retention), so a job with no requester reads
 * "Scheduled by the server" rather than a deleted account's (BRIDGE-03). A
 * follow (podcast.subscribe) always has a listener behind it.
 */
export function serverScheduled(kind: string): boolean {
  return kind.startsWith('podcast.') && kind !== 'podcast.subscribe';
}
