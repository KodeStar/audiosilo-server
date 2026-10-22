import type { TFunction } from 'i18next';
import type {
  AdminSettings,
  MetadataMode,
  SettingsPatch,
  SettingsSection,
  SystemStatus,
} from '@/api/types';

// Server > Settings: which sections exist, what each setting id is called, and
// the small rules the forms share (locked, restart, list editing, diffs). Each
// setting lives in exactly one section (STYLEGUIDE.md section 2).

export const SETTINGS_PAGES = [
  'general',
  'network',
  'players',
  'metadata',
  'transcoding',
  'household',
  'demo',
  'backups',
  'notifications',
  'import',
  'bridge',
  'modules',
] as const;
export type SettingsPage = (typeof SETTINGS_PAGES)[number];

/**
 * The settings pages a managed module brings, by page: the module id whose
 * toggle shows it (Settings > Modules). None yet: Bridge Phase 2b's
 * contributions module is the first to add one.
 */
export const MODULE_PAGES: Partial<Record<SettingsPage, string>> = {};

/**
 * The pages the nav lists: every page, less those whose module is off (or
 * unknown to the server, so absent from settings.modules).
 */
export function visiblePages(
  pages: readonly SettingsPage[],
  settings: Pick<AdminSettings, 'modules'> | undefined,
  modulePages: Partial<Record<SettingsPage, string>> = MODULE_PAGES,
): SettingsPage[] {
  return pages.filter((p) => {
    const id = modulePages[p];
    return id === undefined || settings?.modules?.[id] === true;
  });
}

/** Settings > Community metadata's source choices, the default (the service) first. */
export const METADATA_MODES = ['remote', 'mirror'] as const satisfies readonly MetadataMode[];

/** A setting's id: "<section>.<name>", as the server names it. */
export type SettingId = {
  [S in SettingsSection]: `${S}.${Exclude<Extract<keyof AdminSettings[S], string>, ReadOnlyFact>}`;
}[SettingsSection];

/** Facts the envelope carries beside the settings; not settings themselves. */
type ReadOnlyFact =
  | 'available'
  | 'web_player'
  | 'max_users_default'
  | 'max_members_default'
  | 'max_members_min'
  | 'max_members_max';

/** Why the console can't change a setting, if it can't: the variable that sets it, or "launcher". */
export function lockOf(s: AdminSettings, id: SettingId): string | undefined {
  return s.locked[id];
}

/** What a lock says: "Set by AUDIOSILO_X" or "Managed by the desktop app". */
export function lockLabel(t: TFunction, lock: string): string {
  return lock === 'launcher'
    ? t('settings.locked.launcher')
    : t('settings.locked.env', { name: lock });
}

/** The setting is read only when the server starts. */
export function needsRestart(s: AdminSettings, id: SettingId): boolean {
  return s.restart_settings.includes(id);
}

/** One entry per line (commas also split), trimmed, blanks dropped. */
export function textToList(text: string): string[] {
  return text
    .split(/[\n,]/)
    .map((l) => l.trim())
    .filter(Boolean);
}

export function listToText(list: readonly string[]): string {
  return list.join('\n');
}

/**
 * Whether a number field's text is outside its whole-number range, checked
 * before saving (the server refuses the same). Blank is too: a ranged setting
 * always holds a number (the server refuses null for it, "enter a value").
 */
export function outOfRange(text: string, min: number, max: number): boolean {
  const s = text.trim();
  if (s === '') return true;
  const n = Number(s);
  return !Number.isInteger(n) || n < min || n > max;
}

function same(a: unknown, b: unknown): boolean {
  if (Array.isArray(a) && Array.isArray(b)) {
    return a.length === b.length && a.every((v, i) => v === b[i]);
  }
  return a === b;
}

/**
 * The fields of `draft` that differ from `saved`, as one section's PATCH body
 * (undefined when nothing changed).
 */
export function sectionPatch<S extends SettingsSection>(
  section: S,
  saved: AdminSettings[S],
  draft: Partial<AdminSettings[S]>,
): SettingsPatch | undefined {
  const changed: Partial<AdminSettings[S]> = {};
  let any = false;
  for (const key of Object.keys(draft) as (keyof AdminSettings[S])[]) {
    if (!same(draft[key], saved[key])) {
      changed[key] = draft[key];
      any = true;
    }
  }
  return any ? ({ [section]: changed } as SettingsPatch) : undefined;
}

/** The setting ids a PATCH body names. */
export function patchIds(patch: SettingsPatch): SettingId[] {
  return Object.entries(patch).flatMap(([section, fields]) =>
    Object.keys(fields ?? {}).map((name) => `${section}.${name}` as SettingId),
  );
}

/** How many days until `iso` (negative once past), whole days rounded down. */
function daysUntil(iso: string, now: number): number {
  return Math.floor((Date.parse(iso) - now) / 86_400_000);
}

/** What the served certificate's state means: fine, worth a look, a problem, or not issued yet. */
export type CertStatus = 'ok' | 'warn' | 'bad' | 'waiting';

/**
 * How the served certificate reads (null with plain HTTP): its i18n key, what
 * that means, and the days it has left. Settings shows it as a badge, Health as a row.
 */
export function certificateLook(
  tls: SystemStatus['tls'],
  now: number,
): { key: string; status: CertStatus; days?: number } | null {
  if (tls.mode === 'off') return null;
  if (tls.error) return { key: 'settings.network.cert.unreadable', status: 'warn' };
  const issued = tls.certificates.filter((c) => c.issued);
  if (issued.length === 0) return { key: 'settings.network.cert.pending', status: 'waiting' };
  const days = Math.min(...issued.map((c) => daysUntil(c.not_after, now)));
  if (days < 0) return { key: 'settings.network.cert.expired', status: 'bad' };
  if (days < 14) return { key: 'settings.network.cert.expiring', status: 'warn', days };
  return { key: 'settings.network.cert.valid', status: 'ok', days };
}
