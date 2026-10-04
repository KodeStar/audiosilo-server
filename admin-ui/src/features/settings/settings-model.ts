import type { AdminSettings, SettingsPatch, SettingsSection, SystemStatus } from '@/api/types';

// Server > Settings: which sections exist, what each setting id is called, and
// the small rules the forms share (locked, restart, list editing, diffs). Each
// setting lives in exactly one section (STYLEGUIDE.md section 2).

export const SETTINGS_PAGES = [
  'general',
  'network',
  'players',
  'metadata',
  'transcoding',
  'demo',
] as const;
export type SettingsPage = (typeof SETTINGS_PAGES)[number];

/** A setting's id: "<section>.<name>", as the server names it. */
export type SettingId = {
  [S in SettingsSection]: `${S}.${Extract<keyof AdminSettings[S], string>}`;
}[SettingsSection];

/** Why the console can't change a setting, if it can't: the variable that sets it, or "launcher". */
export function lockOf(s: AdminSettings, id: SettingId): string | undefined {
  return s.locked[id];
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
export function daysUntil(iso: string, now: number = Date.now()): number {
  return Math.floor((Date.parse(iso) - now) / 86_400_000);
}

/** How the served certificate reads: valid, expiring soon, expired, or not issued yet. */
export function certificateLook(
  tls: SystemStatus['tls'],
  now: number,
): {
  key: string;
  tone: 'success' | 'warning' | 'destructive' | 'secondary';
  days?: number;
} | null {
  if (tls.mode === 'off') return null;
  if (tls.error) return { key: 'settings.network.cert.unreadable', tone: 'warning' };
  const issued = tls.certificates.filter((c) => c.issued);
  if (issued.length === 0) return { key: 'settings.network.cert.pending', tone: 'secondary' };
  const days = Math.min(...issued.map((c) => daysUntil(c.not_after, now)));
  if (days < 0) return { key: 'settings.network.cert.expired', tone: 'destructive' };
  if (days < 14) return { key: 'settings.network.cert.expiring', tone: 'warning', days };
  return { key: 'settings.network.cert.valid', tone: 'success', days };
}
