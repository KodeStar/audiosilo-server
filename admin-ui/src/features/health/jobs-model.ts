import type { RunEvent, ScanRun, ScanRunStatus } from '@/api/types';
import type { Phrase } from './issues-model';

// The Jobs page's wording rules: what a recorded scan found, how it ended, and
// each log line. Phrases are i18n keys with values, worded where they're shown.

/** A status dot's tone (`.dot[data-tone]`): ok, warn, bad, or live while running. */
export function statusTone(status: ScanRunStatus): 'ok' | 'warn' | 'bad' | 'live' {
  switch (status) {
    case 'running':
      return 'live';
    case 'ok':
      return 'ok';
    case 'partial':
    case 'cancelled':
    case 'interrupted':
      return 'warn';
    case 'unavailable':
    case 'failed':
      return 'bad';
  }
}

/**
 * What a scan found, as phrases joined with " · ": the changes for a scan that
 * finished (or "no changes"), else how it ended (always saying nothing was
 * removed when nothing was).
 */
export function runSummary(
  run: Pick<ScanRun, 'status' | 'added' | 'updated' | 'moved' | 'removed' | 'errors'>,
): Phrase[] {
  if (run.status !== 'ok' && run.status !== 'partial') {
    return [{ key: `jobs.status.${run.status}` }];
  }
  const parts: Phrase[] = [];
  for (const k of ['added', 'updated', 'moved', 'removed', 'errors'] as const) {
    if (run[k]) parts.push({ key: `jobs.count.${k}`, values: { count: run[k] } });
  }
  if (run.status === 'partial') parts.push({ key: 'jobs.status.partial' });
  return parts.length ? parts : [{ key: 'jobs.noChanges' }];
}

/** How long a finished scan took, in seconds (undefined while it runs). */
export function runSeconds(run: Pick<ScanRun, 'started_at' | 'finished_at'>): number | undefined {
  if (!run.finished_at) return undefined;
  const ms = Date.parse(run.finished_at) - Date.parse(run.started_at);
  return Number.isNaN(ms) ? undefined : Math.max(0, ms / 1000);
}

const unit = (n: number, u: 'hour' | 'minute' | 'second', lang: string, digits = 0) =>
  new Intl.NumberFormat(lang, {
    style: 'unit',
    unit: u,
    unitDisplay: 'narrow',
    maximumFractionDigits: digits,
  }).format(n);

/** "0.4s", "22s", "1m 52s", "1h 3m": a scan's length, in the language's units. */
export function formatTook(seconds: number, lang: string): string {
  if (seconds < 9.95) return unit(Math.round(seconds * 10) / 10, 'second', lang, 1);
  const s = Math.round(seconds);
  if (s < 60) return unit(s, 'second', lang);
  const m = Math.floor(s / 60);
  if (m < 60) return `${unit(m, 'minute', lang)} ${unit(s % 60, 'second', lang)}`;
  return `${unit(Math.floor(m / 60), 'hour', lang)} ${unit(m % 60, 'minute', lang)}`;
}

/** The log event kinds library.runLog writes; anything else reads as "other". */
const EVENT_KINDS = new Set([
  'started',
  'discovered',
  'unreadable',
  'moved',
  'problem',
  'error',
  'removed',
  'partial',
  'unavailable',
  'failed',
  'cancelled',
  'finished',
  'truncated',
]);

/**
 * A log line in words. `problem` names a read problem by its code, which
 * `codeWord` puts into words (the log line embeds it).
 */
export function eventPhrase(e: RunEvent, codeWord: (code: string) => string): Phrase {
  // A problem with the tool's own message words it in brackets after the problem.
  const kind = e.kind === 'problem' && e.detail ? 'problemDetail' : e.kind;
  return {
    key: EVENT_KINDS.has(e.kind) ? `jobs.event.${kind}` : 'jobs.event.other',
    values: {
      kind: e.kind,
      path: e.path ?? '',
      to: e.to ?? '',
      detail: e.detail ?? '',
      count: e.count ?? 0,
      problem: e.code ? codeWord(e.code) : '',
    },
  };
}
