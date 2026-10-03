import type { AuthCode, Library, ListeningRow, PathRule, Share } from '@/api/types';
import { LIVE_WINDOW_MS } from '@/features/overview/overview-model';

// Pure logic behind the People screens, kept out of components so it's tested.

/** A person page's tabs, in order; the first has no ?tab= in the URL. */
export const USER_TABS = ['access', 'invites', 'sign-in', 'account'] as const;
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

/** What a person is in the middle of: their newest unfinished book, live or not. */
export function currentBook(rows: readonly ListeningRow[], userId: number, now: number) {
  const mine = rows
    .filter((r) => r.user_id === userId && !r.finished)
    .sort((a, b) => Date.parse(b.updated_at) - Date.parse(a.updated_at));
  const row = mine[0];
  if (!row) return undefined;
  return { row, live: now - Date.parse(row.updated_at) <= LIVE_WINDOW_MS };
}
