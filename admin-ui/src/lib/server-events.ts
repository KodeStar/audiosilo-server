import type { ServerEvent, ServerEventKind } from '@/api/types';
import { KIND_DESCRIBE, KIND_GENERATE, parkKey } from '@/features/bridge/generate-model';
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

export function describeEvent(ev: ServerEvent): EventText {
  const d = ev.data;
  switch (ev.kind) {
    case 'book_added': {
      const count = num(d.count);
      const titles = (Array.isArray(d.titles) ? d.titles : []).map(str).filter(Boolean);
      const shown = titles.slice(0, TITLES_SHOWN);
      const more = count - shown.length;
      return {
        title: { key: 'events.book_added.title', values: { count, library: str(d.library) } },
        body: shown.length
          ? more > 0
            ? {
                key: 'events.book_added.bodyMore',
                values: { titles: shown.join(', '), count: more },
              }
            : { key: 'events.book_added.body', values: { titles: shown.join(', ') } }
          : undefined,
        tone: 'info',
        link: { destination: 'library' },
      };
    }
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
]);

/**
 * A Bridge job that ended (notify.BridgeJobEnded): characters and recaps, or a
 * description, ready for a book (FINISHED_OWN), another kind finished, or a job
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
    return own
      ? { title: { key: own, values: { name } }, tone: 'info', link }
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
      body: code ? { key: parkKey(code), values: { code } } : detailBody,
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
