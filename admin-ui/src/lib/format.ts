// Locale-aware formatting. Pass the active i18n language so numbers and times
// follow the console's language rather than the browser's.

// Intl formatters are costly to build and a grid of hundreds formats the same
// way per cell: keep one per language and options.
const formatters = new Map<string, Intl.NumberFormat>();

function numberFormat(lang: string, opts: Intl.NumberFormatOptions = {}): Intl.NumberFormat {
  const key = `${lang}|${JSON.stringify(opts)}`;
  let f = formatters.get(key);
  if (!f) formatters.set(key, (f = new Intl.NumberFormat(lang, opts)));
  return f;
}

/** 3,249 / 3.249 / 3 249: thousands separators, tabular in the UI via CSS. */
export function formatNumber(n: number, lang: string): string {
  return numberFormat(lang).format(n);
}

/** A count for a plural key: `t('x.books', counted(n, lang))` reads "3,249 books". */
export function counted(n: number, lang: string): { count: number; formatted: string } {
  return { count: n, formatted: formatNumber(n, lang) };
}

/** 0.62 → "62%". Clamped to 0..1 so a stale position can't read 104%. */
export function formatPercent(fraction: number, lang: string): string {
  const f = Number.isFinite(fraction) ? Math.min(1, Math.max(0, fraction)) : 0;
  return numberFormat(lang, { style: 'percent', maximumFractionDigits: 0 }).format(f);
}

const unit = (n: number, u: 'hour' | 'minute' | 'second', lang: string, digits = 0) =>
  numberFormat(lang, {
    style: 'unit',
    unit: u,
    unitDisplay: 'narrow',
    maximumFractionDigits: digits,
  }).format(n);

/**
 * A length as "27h 18m" ("45m" under an hour, "40s" under a minute), rounded
 * to the nearest minute, in the language's units. "" when unknown (0 or less).
 */
export function formatDuration(seconds: number, lang: string): string {
  if (!Number.isFinite(seconds) || seconds <= 0) return '';
  if (seconds < 59.5) return unit(Math.max(1, Math.round(seconds)), 'second', lang);
  const total = Math.round(seconds / 60);
  const h = Math.floor(total / 60);
  const m = total % 60;
  if (!h) return unit(m, 'minute', lang);
  return m ? `${unit(h, 'hour', lang)} ${unit(m, 'minute', lang)}` : unit(h, 'hour', lang);
}

/** "0.4s", "22s", "1m 52s", "1h 3m": how long a job took, to the second. */
export function formatTook(seconds: number, lang: string): string {
  if (seconds < 9.95) return unit(Math.round(seconds * 10) / 10, 'second', lang, 1);
  const s = Math.round(seconds);
  if (s < 60) return unit(s, 'second', lang);
  const m = Math.floor(s / 60);
  if (m < 60) return `${unit(m, 'minute', lang)} ${unit(s % 60, 'second', lang)}`;
  return `${unit(Math.floor(m / 60), 'hour', lang)} ${unit(m % 60, 'minute', lang)}`;
}

const BYTE_UNITS = ['byte', 'kilobyte', 'megabyte', 'gigabyte', 'terabyte'] as const;

/** "1.3 GB": decimal units, one decimal under 10. */
export function formatBytes(bytes: number, lang: string): string {
  let v = Math.max(0, bytes);
  let i = 0;
  while (v >= 1000 && i < BYTE_UNITS.length - 1) {
    v /= 1000;
    i++;
  }
  return numberFormat(lang, {
    style: 'unit',
    unit: BYTE_UNITS[i],
    unitDisplay: 'short',
    maximumFractionDigits: v < 10 && i > 0 ? 1 : 0,
  }).format(v);
}

/** A position as "1:02:05" (or "2:05" under an hour unless `withHours`). */
export function formatClock(seconds: number, withHours = seconds >= 3600): string {
  const s = Math.max(0, Math.round(seconds));
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const sec = String(s % 60).padStart(2, '0');
  return withHours || h > 0 ? `${h}:${String(m).padStart(2, '0')}:${sec}` : `${m}:${sec}`;
}

/** A translate function (i18next's `t`, or a stub in tests). */
type Translate = (key: string, opts: Record<string, unknown>) => string;

/** A position in a series as "#2" ("" for none, 0). */
export function seriesIndexLabel(index: number, lang: string, t: Translate): string {
  return index > 0 ? t('books.tile.seriesIndex', { index: formatNumber(index, lang) }) : '';
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

const clockFormats = new Map<string, Intl.DateTimeFormat>();

/** A time of day to the second ("10:20:19"), for log lines. "" if unparsable. */
export function formatClockTime(iso: string, lang: string): string {
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return '';
  let f = clockFormats.get(lang);
  if (!f) {
    f = new Intl.DateTimeFormat(lang, { hour: '2-digit', minute: '2-digit', second: '2-digit' });
    clockFormats.set(lang, f);
  }
  return f.format(t);
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
