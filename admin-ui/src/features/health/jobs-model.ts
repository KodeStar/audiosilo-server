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

/** The log event kinds library.runLog writes; anything else reads as "other". */
const EVENT_KINDS = new Set([
  'started',
  'discovered',
  'unreadable',
  'moved',
  'joined',
  'split',
  'problem',
  'error',
  'removed',
  'partial',
  'unavailable',
  'failed',
  'cancelled',
  'interrupted',
  'finished',
  'truncated',
]);

/**
 * A log line in words. `problem` names a read problem by its code, which
 * `codeWord` puts into words (the log line embeds it). A `joined` disc whose
 * length was unknown (code `length_unknown`) kept its progress, and says so.
 */
export function eventPhrase(e: RunEvent, codeWord: (code: string) => string): Phrase {
  // A problem with the tool's own message words it in brackets after the problem.
  const kind =
    e.kind === 'problem' && e.detail
      ? 'problemDetail'
      : e.kind === 'joined' && e.code === 'length_unknown'
        ? 'joinedKept'
        : e.kind;
  return {
    key: EVENT_KINDS.has(e.kind) ? `jobs.event.${kind}` : 'jobs.event.other',
    values: {
      kind: e.kind,
      path: e.path ?? '',
      to: e.to ?? '',
      detail: e.detail ?? '',
      count: e.count ?? 0,
      problem: e.kind === 'problem' && e.code ? codeWord(e.code) : '',
    },
  };
}
