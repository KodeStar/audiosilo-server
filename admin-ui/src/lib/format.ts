// Locale-aware formatting. Pass the active i18n language so numbers and times
// follow the console's language rather than the browser's.

/** 3,249 / 3.249 / 3 249: thousands separators, tabular in the UI via CSS. */
export function formatNumber(n: number, lang: string): string {
  return new Intl.NumberFormat(lang).format(n);
}

/** 0.62 → "62%". Clamped to 0..1 so a stale position can't read 104%. */
export function formatPercent(fraction: number, lang: string): string {
  const f = Number.isFinite(fraction) ? Math.min(1, Math.max(0, fraction)) : 0;
  return new Intl.NumberFormat(lang, { style: 'percent', maximumFractionDigits: 0 }).format(f);
}

const UNITS: [Intl.RelativeTimeFormatUnit, number][] = [
  ['year', 365 * 24 * 3600],
  ['month', 30 * 24 * 3600],
  ['week', 7 * 24 * 3600],
  ['day', 24 * 3600],
  ['hour', 3600],
  ['minute', 60],
];

/**
 * "14 min ago" / "yesterday" for recency. Anything under a minute reads "now".
 * `now` is injectable for tests.
 */
export function formatRelative(iso: string, lang: string, now: number = Date.now()): string {
  const then = Date.parse(iso);
  if (Number.isNaN(then)) return '';
  const secs = Math.round((then - now) / 1000);
  const rtf = new Intl.RelativeTimeFormat(lang, { numeric: 'auto', style: 'short' });
  for (const [unit, size] of UNITS) {
    if (Math.abs(secs) >= size) return rtf.format(Math.round(secs / size), unit);
  }
  return rtf.format(0, 'second');
}

/** A moment as a record ("Oct 10, 7:52 PM"): absolute, unlike recency. "" if unparsable. */
export function formatDateTime(iso: string, lang: string): string {
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return '';
  return new Intl.DateTimeFormat(lang, {
    month: 'short',
    day: 'numeric',
    hour: 'numeric',
    minute: '2-digit',
  }).format(t);
}

/** Today's date as the Overview eyebrow: "Saturday 3 October". */
export function formatLongDate(date: Date, lang: string): string {
  return new Intl.DateTimeFormat(lang, { weekday: 'long', day: 'numeric', month: 'long' }).format(
    date,
  );
}

/** Position as a fraction of duration, 0 when the duration is unknown. */
export function progressFraction(position: number, duration: number): number {
  return duration > 0 ? Math.min(1, Math.max(0, position / duration)) : 0;
}

/** "0.9.2" → "v0.9.2"; a non-numeric build label ("dev", "dev-1a2b") stays as is. */
export function formatVersion(version: string): string {
  return /^\d/.test(version) ? `v${version}` : version;
}
