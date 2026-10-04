import { sortLive } from '@/features/activity/live-model';
import type {
  AuthCode,
  Device,
  Library,
  ListeningRow,
  ListeningSession,
  PathRule,
  ProgressEdit,
  Share,
} from '@/api/types';

// Pure logic behind the People screens, kept out of components so it's tested.

/** A person page's tabs, in order; the first has no ?tab= in the URL. */
export const USER_TABS = [
  'listening',
  'access',
  'devices',
  'invites',
  'sign-in',
  'account',
] as const;
export type UserTab = (typeof USER_TABS)[number];

/**
 * Where an invite stands. An invite stays `active` (still pairs devices) until it
 * expires or its uses run out; `uses` counts devices that actually paired.
 */
export type InviteStatus = 'active' | 'usedUp' | 'expired';

export function inviteStatus(code: AuthCode, now: number): InviteStatus {
  if (code.expires_at && Date.parse(code.expires_at) <= now) return 'expired';
  if (code.max_uses > 0 && code.uses >= code.max_uses) return 'usedUp';
  return 'active';
}

/** Active invites first, then newest first within each group. */
export function sortInvites<T extends AuthCode>(codes: readonly T[], now: number): T[] {
  const rank = (c: T) => (inviteStatus(c, now) === 'active' ? 0 : 1);
  return [...codes].sort(
    (a, b) => rank(a) - rank(b) || Date.parse(b.created_at) - Date.parse(a.created_at),
  );
}

/** The library a whole-library grant gives (the server marks those shares). */
export function wholeLibraryOf(share: Share): number | undefined {
  return share.whole_library_id;
}

/**
 * Something to give a person, as a radio value: a whole library or a share.
 * One encoding, read back by `parseAccessChoice`.
 */
export type AccessChoice = { kind: 'library' | 'share'; id: number };
export const accessValue = (c: AccessChoice) => `${c.kind}:${c.id}`;
export function parseAccessChoice(value: string): AccessChoice | undefined {
  const [kind, id] = value.split(':');
  return (kind === 'library' || kind === 'share') && Number(id) > 0
    ? { kind, id: Number(id) }
    : undefined;
}

/**
 * What to call a share: a whole-library grant goes by its library's name (the
 * server names it "Library: <name>" internally), anything else by its own name.
 */
export function shareLabel(share: Share, libraries: readonly Pick<Library, 'id' | 'name'>[]) {
  const id = wholeLibraryOf(share);
  return (id !== undefined && libraries.find((l) => l.id === id)?.name) || share.name;
}

/** "Fiction" for a whole-library rule, "Fiction › Kids/Gruffalo" for a folder. */
export function ruleLabel(rule: PathRule, libraries: readonly Pick<Library, 'id' | 'name'>[]) {
  const lib = libraries.find((l) => l.id === rule.library_id)?.name ?? `#${rule.library_id}`;
  return rule.path ? `${lib} › ${rule.path}` : lib;
}

/** A moment as the value of a date input (YYYY-MM-DD, the browser's day); "" for none. */
export function dateInputValue(iso: string | null | undefined): string {
  const t = iso ? Date.parse(iso) : NaN;
  if (Number.isNaN(t)) return '';
  const d = new Date(t);
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
}

/** Why a start/finish date pair can't be saved (an i18n key), or undefined when it can. */
export function datesProblem(started: string, finished: string, today: string) {
  if (started > today || finished > today) return 'progress.dates.future';
  if (started && finished && finished < started) return 'progress.dates.order';
  return undefined;
}

/**
 * The edit a dates form makes: only the dates that changed, each a YYYY-MM-DD day
 * (the start of that day, server time) or null to clear it.
 */
export function datesEdit(
  before: { started: string; finished: string },
  after: { started: string; finished: string },
): ProgressEdit {
  const edit: ProgressEdit = {};
  if (after.started !== before.started) edit.started_at = after.started || null;
  if (after.finished !== before.finished) edit.finished_at = after.finished || null;
  return edit;
}

/** A person card's book: what they are listening to now, else their newest unfinished book. */
export interface CurrentBook {
  library_id: number;
  path: string;
  title: string;
  position: number;
  duration: number;
  /** A live session (playing or paused in the last ten minutes) on some device. */
  live: boolean;
}

export function currentBook(
  rows: readonly ListeningRow[],
  live: readonly ListeningSession[],
  userId: number,
): CurrentBook | undefined {
  const session = sortLive(live.filter((s) => s.user_id === userId))[0];
  if (session) return { ...session, live: true };
  const row = rows
    .filter((r) => r.user_id === userId && !r.finished)
    .sort((a, b) => Date.parse(b.updated_at) - Date.parse(a.updated_at))[0];
  return row ? { ...row, live: false } : undefined;
}

/** Each person's paired devices (sessions, not API keys), by user id; undefined while loading. */
/** The signed-in devices (sessions) among a list, without the API keys. */
export function signedIn(devices: readonly Device[]): Device[] {
  return devices.filter((d) => d.kind === 'session');
}

export function groupDevices(devices: readonly Device[] | undefined) {
  if (!devices) return undefined;
  const out = new Map<number, Device[]>();
  for (const d of signedIn(devices)) {
    const list = out.get(d.user_id);
    if (list) list.push(d);
    else out.set(d.user_id, [d]);
  }
  return out;
}
