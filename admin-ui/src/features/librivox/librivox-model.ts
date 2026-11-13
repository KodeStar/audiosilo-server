import type { BridgeJob, BridgeStatus } from '@/api/types';
import { isOpen, text } from '@/features/bridge/bridge-model';

// Library > LibriVox (LibriVox L6): what the page decides, kept out of the
// components so the rules are tested on their own.

/** The Bridge's job that adds a LibriVox project to a library. */
export const LIBRIVOX_KIND = 'librivox.fetch';
/** The Bridge feature that offers it (the kind's family, GET /admin/bridge features). */
export const LIBRIVOX_FEATURE = 'librivox';

/**
 * Why the Bridge can't take an Add now, else undefined: the status is still
 * loading or couldn't be read, no Bridge is paired, or the paired one doesn't
 * run librivox.fetch (an older Bridge, or one that hasn't reported yet). An
 * offline Bridge is no block: the job waits for it.
 */
export type AddBlock = 'loading' | 'error' | 'unpaired' | 'unsupported';

export function addBlock(
  status: Pick<BridgeStatus, 'paired' | 'features'> | undefined,
  isError: boolean,
): AddBlock | undefined {
  if (!status) return isError ? 'error' : 'loading';
  if (!status.paired) return 'unpaired';
  if (!status.features.includes(LIBRIVOX_FEATURE)) return 'unsupported';
  return undefined;
}

/** The Bridge note under the search: a block, or that an offline Bridge's jobs wait. */
export function bridgeNote(
  status: Pick<BridgeStatus, 'paired' | 'features' | 'online'> | undefined,
  isError: boolean,
): AddBlock | 'offline' | undefined {
  const block = addBlock(status, isError);
  if (block) return block;
  return status?.online ? undefined : 'offline';
}

/** The failure codes a librivox.fetch may name (bridge.LibrivoxFetchCodes). */
export const FETCH_CODES = [
  'not_writable',
  'exists',
  'disk_full',
  'not_found',
  'archive_unavailable',
  'metadata_off',
] as const;

/**
 * The i18n key of a librivox.fetch's known failure code, else undefined: a
 * park's code, else the result's code, else an error that starts with one
 * ("exists: ..."), as the server reads it (bridge.LibrivoxFetchCode).
 */
export function fetchReason(
  job: Pick<BridgeJob, 'kind' | 'state' | 'progress' | 'result' | 'error'>,
): string | undefined {
  if (job.kind !== LIBRIVOX_KIND) return undefined;
  const known = (code: string | undefined) =>
    code && (FETCH_CODES as readonly string[]).includes(code) ? code : undefined;
  let code: string | undefined;
  if (job.state === 'needs_attention') {
    const park = job.progress?.park;
    code = known(park && typeof park === 'object' ? text((park as { code?: unknown }).code) : '');
  } else if (job.state === 'failed' || job.state === 'cancelled') {
    const own = text(job.result?.code);
    code = own
      ? known(own)
      : FETCH_CODES.find((c) => job.error === c || job.error.startsWith(`${c}:`));
  }
  return code ? `librivox.code.${code}` : undefined;
}

/** The LibriVox project a librivox.fetch adds (its params), for the job list. */
export function librivoxProject(job: Pick<BridgeJob, 'kind' | 'params'>): number | undefined {
  if (job.kind !== LIBRIVOX_KIND) return undefined;
  const id = job.params?.librivox_id;
  return typeof id === 'number' && Number.isInteger(id) && id > 0 ? id : undefined;
}

/** How often a tracked job is asked again: every few seconds while it is open. */
export const JOB_POLL_MS = 3000;

export function jobPollMs(job: Pick<BridgeJob, 'state'> | undefined): number | false {
  return job && isOpen(job.state) ? JOB_POLL_MS : false;
}
