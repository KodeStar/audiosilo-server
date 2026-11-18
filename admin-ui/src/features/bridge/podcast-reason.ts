import type { BridgeJob } from '@/api/types';
import { feedCodeText, httpStatusOf } from '@/lib/server-events';
import { text } from './bridge-model';

// A failed podcast job worded the server's way, not the Bridge's: the codes
// the server has a sentence of its own for. The Bridge's text stays in the
// job's Details (its result's message).

/** The codes of a podcast.fetch the server words (bridge.fetchMessages; each has a `bridge.podcastCode.<code>`). */
export const PODCAST_FETCH_CODES: ReadonlySet<string> = new Set([
  'dns',
  'network',
  'timeout',
  'tls',
  'invalid_feed_url',
  'not_a_feed',
  'not_a_podcast',
  'feed_secret_missing',
  'write_failed',
  'not_podcast_library',
  'identity_changed',
  'gone',
  'too_large',
  'episode_not_found',
  'enclosure_missing',
  'invalid_audio',
  'probe_unavailable',
  'unsupported_encoding',
  'index_failed',
  'module_off',
  'bridge_unavailable',
  'library_not_writable',
  'demo_account',
  'fetch_limit',
  'blocked_address',
  'fetch_failed',
  'bridge_lost',
  'redirect_loop',
]);

/** The codes of a podcast.poll that aren't the feed's (notify words none) but read the same as a fetch's. */
const POLL_OWN_CODES = new Set([
  'bridge_lost',
  'library_not_writable',
  'not_podcast_library',
  'module_off',
]);

export interface PodcastReason {
  key: string;
  values?: Record<string, string | number>;
}

/**
 * The server's wording of a failed podcast.fetch or podcast.poll, else
 * undefined (the Bridge's own text then). The code is the result's, and a
 * failed job without one is bridge_lost, as the server reads it
 * (bridge.PodcastOutcomeOf). A fetch words its codes as the player's status
 * does (bridge.FetchErrorFor), a poll as the failing-feed notice does
 * (notify's feedCodeText).
 */
export function podcastJobReason(
  job: Pick<BridgeJob, 'kind' | 'state' | 'result'>,
): PodcastReason | undefined {
  if (job.kind !== 'podcast.fetch' && job.kind !== 'podcast.poll') return undefined;
  if (job.state !== 'failed') return undefined;
  const code = text(job.result?.code) ?? 'bridge_lost';
  if (job.kind === 'podcast.poll') {
    if (POLL_OWN_CODES.has(code)) return { key: `bridge.podcastCode.${code}` };
    return feedCodeText(code);
  }
  if (code === 'http_310') return { key: 'bridge.podcastCode.redirect_loop' }; // an older Bridge's redirect loop
  if (PODCAST_FETCH_CODES.has(code)) return { key: `bridge.podcastCode.${code}` };
  const status = httpStatusOf(code);
  if (status) return { key: 'bridge.podcastCode.http', values: { status } };
  return undefined;
}
