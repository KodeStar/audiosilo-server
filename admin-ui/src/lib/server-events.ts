import type { ServerEvent, ServerEventKind } from '@/api/types';
import {
  coverageDown,
  KIND_DESCRIBE,
  KIND_DETECT,
  KIND_GENERATE,
  KIND_READALONG,
  parkKey,
} from '@/features/bridge/generate-model';
import { LIBRIVOX_KIND, fetchParkKey } from '@/features/librivox/librivox-model';
import { reasonKey } from '@/features/people/pending-model';
import type { SettingsPage } from '@/features/settings/settings-model';
import { readStorage, writeStorage } from './storage';

// The server's event feed (GET /admin/events) as the console words it: the bell
// in the top bar, and the event names Settings > Notifications subscribes to.
// Pure apart from the "seen" cursor, so the wording rules are tested on their own.

/** Every kind, in the server's order (notify.Kinds). */
export const EVENT_KINDS: readonly ServerEventKind[] = [
  'book_added',
  'scan_failed',
  'library_unavailable',
  'new_device',
  'invite_redeemed',
  'update_available',
  'backup_failed',
  'bridge_job_finished',
  'bridge_job_failed',
  'book_released',
  'podcast_episodes_new',
  'podcast_feed_failing',
  'podcast_feed_recovered',
  'oidc_pending',
];

export type EventTone = 'info' | 'warn' | 'bad';

/** Where an event leads in the console (a destination section, maybe a Settings topic). */
export interface EventLink {
  destination: 'library' | 'people' | 'health' | 'server';
  /** undefined = the destination's first section. */
  section?: string;
  topic?: SettingsPage;
}

/** An event in words: i18n keys and their values, how serious it is, where it leads. */
export interface EventText {
  title: { key: string; values?: Record<string, string | number> };
  body?: { key: string; values?: Record<string, string | number> };
  tone: EventTone;
  link: EventLink;
}

const str = (v: unknown): string => (typeof v === 'string' ? v : '');
const num = (v: unknown): number => (typeof v === 'number' ? v : 0);

/** How many added titles the bell lists before "and N more". */
const TITLES_SHOWN = 3;

/** A book list event's body: the first titles, then "and N more" when `count` is more. */
function titlesBody(
  d: ServerEvent['data'],
  prefix: 'events.book_added' | 'events.book_released' | 'events.podcast_episodes_new',
): EventText['body'] {
  const count = num(d.count);
  const titles = (Array.isArray(d.titles) ? d.titles : []).map(str).filter(Boolean);
  const shown = titles.slice(0, TITLES_SHOWN);
  if (!shown.length) return undefined;
  const more = count - shown.length;
  return more > 0
    ? { key: `${prefix}.bodyMore`, values: { titles: shown.join(', '), count: more } }
    : { key: `${prefix}.body`, values: { titles: shown.join(', ') } };
}

export function describeEvent(ev: ServerEvent): EventText {
  const d = ev.data;
  switch (ev.kind) {
    case 'book_added':
      return {
        title: {
          key: 'events.book_added.title',
          values: { count: num(d.count), library: str(d.library) },
        },
        body: titlesBody(d, 'events.book_added'),
        tone: 'info',
        link: { destination: 'library' },
      };
    case 'book_released':
      // notify.BooksReleased: the titles come "Title (Series #n)", at most five.
      return {
        title: { key: 'events.book_released.title', values: { count: num(d.count) } },
        body: titlesBody(d, 'events.book_released'),
        tone: 'info',
        link: { destination: 'library', section: 'series' },
      };
    case 'scan_failed':
      return {
        title: { key: 'events.scan_failed.title', values: { library: str(d.library) } },
        body: str(d.detail)
          ? { key: 'events.detail', values: { detail: str(d.detail) } }
          : undefined,
        tone: 'bad',
        link: { destination: 'health', section: 'jobs' },
      };
    case 'library_unavailable':
      return {
        title: { key: 'events.library_unavailable.title', values: { library: str(d.library) } },
        body: { key: 'events.library_unavailable.body' },
        tone: 'warn',
        link: { destination: 'library', section: 'libraries' },
      };
    case 'new_device': {
      const device = [str(d.device), str(d.app)].filter(Boolean).join(' · ');
      return {
        title: { key: 'events.new_device.title', values: { user: str(d.user) } },
        body: device ? { key: 'events.detail', values: { detail: device } } : undefined,
        tone: 'info',
        link: { destination: 'people', section: 'devices' },
      };
    }
    case 'invite_redeemed':
      return {
        title: { key: 'events.invite_redeemed.title', values: { user: str(d.user) } },
        body: str(d.device)
          ? { key: 'events.invite_redeemed.body', values: { device: str(d.device) } }
          : undefined,
        tone: 'info',
        link: { destination: 'people', section: 'invites' },
      };
    case 'update_available':
      return {
        title: { key: 'events.update_available.title', values: { version: str(d.version) } },
        body: str(d.name) ? { key: 'events.detail', values: { detail: str(d.name) } } : undefined,
        tone: 'info',
        link: { destination: 'server', section: 'about' },
      };
    case 'backup_failed':
      return {
        title: { key: 'events.backup_failed.title' },
        body: { key: backupFailureKey(str(d.error)) },
        tone: 'bad',
        link: { destination: 'server', topic: 'backups' },
      };
    case 'bridge_job_finished':
    case 'bridge_job_failed':
      return describeBridgeJob(ev.kind, d);
    // notify.PodcastNotice: a show's news, named by its title (never its feed
    // address); a private show's (`private`) carries no names at all, so it is
    // "a private podcast". Podcast libraries are listed in Library > Libraries.
    case 'podcast_episodes_new':
      return {
        title: podcastTitle(d, 'events.podcast_episodes_new', { count: num(d.count) }),
        body: titlesBody(d, 'events.podcast_episodes_new'),
        tone: 'info',
        link: { destination: 'library', section: 'libraries' },
      };
    case 'podcast_feed_failing':
      return {
        title: podcastTitle(d, 'events.podcast_feed_failing'),
        body: feedFailureBody(str(d.code)),
        tone: 'warn',
        link: { destination: 'library', section: 'libraries' },
      };
    case 'podcast_feed_recovered':
      return {
        title: podcastTitle(d, 'events.podcast_feed_recovered'),
        body: { key: 'events.podcast_feed_recovered.body' },
        tone: 'info',
        link: { destination: 'library', section: 'libraries' },
      };
    // notify.OIDCPending: a single sign-on no account took, once per waiting
    // row; named by the person's name or username (never their email), or
    // "someone". The waiting sign-ins are listed on People.
    case 'oidc_pending': {
      const provider = str(d.provider);
      const person = str(d.person);
      return {
        title: person
          ? { key: 'events.oidc_pending.title', values: { person, provider } }
          : { key: 'events.oidc_pending.titleSomeone', values: { provider } },
        body: { key: reasonKey(str(d.reason)) },
        tone: 'warn',
        link: { destination: 'people' },
      };
    }
  }
  // A kind from a newer server: say what it is called.
  return {
    title: { key: 'events.unknown', values: { kind: String(ev.kind) } },
    tone: 'info',
    link: { destination: 'server', topic: 'notifications' },
  };
}

/** The kinds whose finished job has a title of its own (the rest name the kind in the body). */
const FINISHED_OWN = new Map<string, string>([
  [KIND_GENERATE, 'events.bridge_job_finished.generate'],
  [KIND_DESCRIBE, 'events.bridge_job_finished.describe'],
  [KIND_DETECT, 'events.bridge_job_finished.detect'],
  [KIND_READALONG, 'events.bridge_job_finished.readalong'],
  [LIBRIVOX_KIND, 'events.bridge_job_finished.librivox'],
]);

/**
 * A Bridge job that ended (notify.BridgeJobEnded): characters and recaps, a
 * description, chapters, or a read-along (with how much of the text it lines
 * up), ready for a book (FINISHED_OWN), another kind finished, or a job
 * that failed or is parked waiting for someone (its park code as a sentence,
 * else the Bridge's own detail). Each leads to Settings > Bridge, where its
 * jobs are listed.
 */
function describeBridgeJob(
  kind: 'bridge_job_finished' | 'bridge_job_failed',
  d: Record<string, unknown>,
): EventText {
  const jobKind = str(d.job_kind);
  // The book's title, else its path, else the library (a library-wide job).
  const name = str(d.title) || str(d.path) || str(d.library) || jobKind;
  const link: EventLink = { destination: 'server', topic: 'bridge' };
  // A kind this console has no name for reads as its code (bridge.kind.<kind>).
  const kindBody = jobKind
    ? { key: `bridge.kind.${jobKind}`, values: { defaultValue: jobKind } }
    : undefined;
  if (kind === 'bridge_job_finished') {
    const own = FINISHED_OWN.get(jobKind);
    // Found chapters the book doesn't use (a chosen source, or the community's):
    // the event names the book's chapter source then (a server that sends none
    // says nothing more).
    const source = str(d.chapters_source);
    const unused =
      jobKind === KIND_DETECT && source !== '' && source !== 'generated'
        ? { key: 'events.bridge_job_finished.detectUnused' }
        : undefined;
    // A read-along's coverage, rounded down to a whole percent (a server that
    // sends none says nothing more).
    const coverage =
      jobKind === KIND_READALONG && typeof d.coverage === 'number'
        ? {
            key: 'events.bridge_job_finished.readalongCoverage',
            values: { percent: Math.round(coverageDown(d.coverage) * 100) },
          }
        : undefined;
    // A LibriVox book the server couldn't match to its recording, or one it
    // found in the library already with another match, says so (a server that
    // sends no librivox_match, or "matched", says nothing more).
    const match = jobKind === LIBRIVOX_KIND ? str(d.librivox_match) : '';
    const librivox =
      match === 'unmatched'
        ? { key: 'events.bridge_job_finished.librivoxUnmatched' }
        : match === 'kept'
          ? { key: 'events.bridge_job_finished.librivoxKept' }
          : undefined;
    return own
      ? {
          title: { key: own, values: { name } },
          body: unused ?? coverage ?? librivox,
          tone: match === 'unmatched' ? 'warn' : 'info',
          link,
        }
      : {
          title: { key: 'events.bridge_job_finished.title', values: { name } },
          body: kindBody,
          tone: 'info',
          link,
        };
  }
  const detail = str(d.detail);
  const detailBody = detail ? { key: 'events.detail', values: { detail } } : kindBody;
  if (str(d.state) === 'needs_attention') {
    const code = str(d.code);
    return {
      title: { key: 'events.bridge_job_failed.attention', values: { name } },
      // A LibriVox fetch's park codes have words of their own.
      body: code
        ? { key: fetchParkKey(jobKind, code) ?? parkKey(code), values: { code } }
        : detailBody,
      tone: 'warn',
      link,
    };
  }
  return {
    title: { key: 'events.bridge_job_failed.title', values: { name } },
    body: detailBody,
    tone: 'bad',
    link,
  };
}

/** A podcast event's title: the show by name, or a private show unnamed. */
function podcastTitle(
  d: ServerEvent['data'],
  prefix:
    'events.podcast_episodes_new' | 'events.podcast_feed_failing' | 'events.podcast_feed_recovered',
  values?: Record<string, number>,
): EventText['title'] {
  if (d.private === true) return { key: `${prefix}.privateTitle`, values };
  return { key: `${prefix}.title`, values: { ...values, show: str(d.show) } };
}

/** The failed poll codes with a sentence of their own (notify's feedCodeText). */
const FEED_CODES = new Set([
  'gone',
  'feed_secret_missing',
  'dns',
  'network',
  'timeout',
  'tls',
  'not_a_feed',
  'not_a_podcast',
  'too_large',
  'blocked_address',
  'invalid_feed_url',
]);

/** Why a podcast feed is failing: its code as a sentence, an HTTP status as such, else generically. */
export function feedFailureBody(code: string): NonNullable<EventText['body']> {
  if (FEED_CODES.has(code)) return { key: `events.podcast_feed_failing.code.${code}` };
  const http = /^http_([1-5]\d{2})$/.exec(code);
  if (http) return { key: 'events.podcast_feed_failing.http', values: { status: http[1] } };
  return { key: 'events.podcast_feed_failing.failed' };
}

/** The i18n key of why a backup failed (backup.Result's error; unknown ones generically). */
export function backupFailureKey(error: string | undefined): string {
  return error === 'disk_full' || error === 'permission_denied'
    ? `backups.failure.${error}`
    : 'backups.failure.failed';
}

const SEEN_KEY = 'audiosilo_events_seen';

/** The newest event id this browser has shown in the open bell (0: none). */
export function readSeen(): number {
  return Number(readStorage(SEEN_KEY)) || 0;
}

export function writeSeen(id: number) {
  writeStorage(SEEN_KEY, String(id));
}

/** How many events are newer than the seen cursor. */
export function unreadCount(events: readonly ServerEvent[], seen: number): number {
  return events.filter((e) => e.id > seen).length;
}
