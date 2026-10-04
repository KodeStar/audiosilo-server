import type {
  ActivityDay,
  ClientCount,
  ClientInfo,
  Funnel,
  PlaybackShare,
  UserProgress,
} from '@/api/types';
import { CLIENT_APP } from '@/api/client';
import { formatDay } from '@/lib/format';

// Pure logic behind the Activity screens (and the People pages' listening year),
// kept out of components so it's tested. Dates are the server's YYYY-MM-DD days:
// they are read as calendar dates (UTC midnight), never shifted into the browser's zone.

/** Seconds as hours (charts plot hours). */
export const toHours = (seconds: number) => seconds / 3600;

/**
 * The change against the previous period as a fraction (0.12 = 12% more), or null
 * when the previous period had nothing to compare with.
 */
export function change(current: number, previous: number): number | null {
  return previous > 0 ? (current - previous) / previous : null;
}

/** A YYYY-MM-DD day as a Date at UTC midnight (format it with timeZone: 'UTC'). */
export const dayDate = (day: string) => new Date(`${day}T00:00:00Z`);

/** Monday = 0 ... Sunday = 6, like the server's hour x weekday grid. */
export const weekdayOf = (day: string) => (dayDate(day).getUTCDay() + 6) % 7;

/** A weekday's name (Monday = 0) in a language: "Mon", or "Monday" when long. */
export function weekdayName(weekday: number, lang: string, width: 'short' | 'long' = 'short') {
  // 2024-01-01 was a Monday.
  return formatDay(`2024-01-0${weekday + 1}`, lang, { weekday: width });
}

/** An hour of the week as "Sat 21:00" ("Saturday 21:00" when long). */
export function slotLabel(weekday: number, hour: number, lang: string, width?: 'short' | 'long') {
  return `${weekdayName(weekday, lang, width)} ${String(hour).padStart(2, '0')}:00`;
}

/** At most this many listeners get their own colour in the hours chart; the rest are "others". */
export const STACKED_LISTENERS = 4;

/** The listeners with the most listening over these days, most first (ties by id). */
export function topListeners(days: readonly ActivityDay[], n = STACKED_LISTENERS): number[] {
  const totals = new Map<number, number>();
  for (const d of days) {
    for (const u of d.by_user) totals.set(u.user_id, (totals.get(u.user_id) ?? 0) + u.listened);
  }
  return [...totals]
    .filter(([, s]) => s > 0)
    .sort((a, b) => b[1] - a[1] || a[0] - b[0])
    .slice(0, n)
    .map(([id]) => id);
}

/** One bar of the hours chart: a day, or a week of a long period. */
export interface HoursBar {
  /** The first and last day it covers (equal for a day). */
  from: string;
  to: string;
  /** Hours per stacked series: `series[i]` is listeners[i], the last entry is everyone else. */
  series: number[];
  total: number;
}

/** Days per bar: one bar a day up to this many days, then one a week. */
export const DAILY_BARS_MAX = 92;

/**
 * The hours chart's bars: a bar a day for short periods, a bar a week (consecutive
 * seven-day runs from the first day; the last may be shorter) for a year, each
 * stacked by `listeners` plus everyone else.
 */
export function hoursBars(days: readonly ActivityDay[], listeners: readonly number[]): HoursBar[] {
  const size = days.length > DAILY_BARS_MAX ? 7 : 1;
  const bars: HoursBar[] = [];
  for (let i = 0; i < days.length; i += size) {
    const chunk = days.slice(i, i + size);
    const series = new Array<number>(listeners.length + 1).fill(0);
    let total = 0;
    for (const d of chunk) {
      total += d.listened;
      let named = 0;
      for (const u of d.by_user) {
        const at = listeners.indexOf(u.user_id);
        if (at >= 0) {
          series[at] += u.listened;
          named += u.listened;
        }
      }
      series[listeners.length] += Math.max(0, d.listened - named);
    }
    bars.push({
      from: chunk[0].date,
      to: chunk[chunk.length - 1].date,
      series: series.map(toHours),
      total: toHours(total),
    });
  }
  return bars;
}

/**
 * Round y-axis ticks from 0 for a largest value (hours): steps of 1, 2 or 5 times a
 * power of ten, at most `count` intervals, so the axis reads 0, 1, 2, 3h rather than
 * 0, 0.9, 1.7, 2.6h.
 */
export function niceTicks(max: number, count = 4): number[] {
  if (!(max > 0)) return [0, 1];
  const raw = max / count;
  const pow = 10 ** Math.floor(Math.log10(raw));
  const step = ([1, 2, 5, 10].find((m) => m * pow >= raw) ?? 10) * pow;
  const n = Math.ceil(max / step - 1e-9);
  return Array.from({ length: n + 1 }, (_, i) => Number((i * step).toPrecision(12)));
}

/** A shade of the one-hue sequential scale (`--seq-0..5`) for a value against the largest. */
export function seqLevel(value: number, max: number): number {
  if (value <= 0 || max <= 0) return 0;
  return Math.min(5, Math.max(1, Math.ceil((value / max) * 5)));
}

export interface CalendarCell {
  date: string;
  listened: number;
  level: number;
}

/**
 * The year heatmap: one column a week (Monday on top), blanks before the first day,
 * and where each month's label sits (the week holding its first day).
 */
export function calendarGrid(days: readonly ActivityDay[]) {
  const max = days.reduce((m, d) => Math.max(m, d.listened), 0);
  const lead = days.length ? weekdayOf(days[0].date) : 0;
  const cells: (CalendarCell | null)[] = [
    ...new Array<null>(lead).fill(null),
    ...days.map((d) => ({ date: d.date, listened: d.listened, level: seqLevel(d.listened, max) })),
  ];
  const weeks: (CalendarCell | null)[][] = [];
  for (let i = 0; i < cells.length; i += 7) weeks.push(cells.slice(i, i + 7));
  const months: { week: number; date: string }[] = [];
  weeks.forEach((week, w) => {
    const first = week.find((c) => c?.date.endsWith('-01'));
    if (first) months.push({ week: w, date: first.date });
  });
  // A period that starts mid-month labels its opening month too, if there's room.
  const opening = days[0]?.date;
  if (opening && !opening.endsWith('-01') && (months[0]?.week ?? Infinity) >= 3) {
    months.unshift({ week: 0, date: opening });
  }
  // A month that only starts in the last two columns has no room for its label.
  while (months.length > 1 && months[months.length - 1].week > weeks.length - 3) months.pop();
  return { weeks, months };
}

/** The busiest hour of the week (weekday 0 = Monday), or null with no listening. */
export function busiestSlot(grid: readonly (readonly number[])[]) {
  let best: { weekday: number; hour: number; listened: number } | null = null;
  grid.forEach((row, weekday) =>
    row.forEach((listened, hour) => {
      if (listened > 0 && (!best || listened > best.listened)) best = { weekday, hour, listened };
    }),
  );
  return best as { weekday: number; hour: number; listened: number } | null;
}

/** The share of started books that were finished, or null when nothing was started. */
export function finishRate(f: Funnel): number | null {
  return f.started > 0 ? f.finished / f.started : null;
}

/** The longest run of consecutive days with listening (the days are consecutive). */
export function longestStreak(days: readonly ActivityDay[]): number {
  let best = 0;
  let run = 0;
  for (const d of days) {
    run = d.listened > 0 ? run + 1 : 0;
    best = Math.max(best, run);
  }
  return best;
}

/** Seconds per calendar month (0 = January) over these days. */
export function monthTotals(days: readonly ActivityDay[]): number[] {
  const out = new Array<number>(12).fill(0);
  for (const d of days) out[Number(d.date.slice(5, 7)) - 1] += d.listened;
  return out;
}

/**
 * How many of a person's progress rows were finished within a year, by their finish
 * date in server time (`utcOffset` minutes), the calendar the listening days use.
 */
export function finishedCount(rows: readonly UserProgress[], year: number, utcOffset: number) {
  return rows.filter(
    (r) =>
      r.finished &&
      r.finished_at &&
      new Date(Date.parse(r.finished_at) + utcOffset * 60_000).getUTCFullYear() === year,
  ).length;
}

/** How a slice of the playback donut is drawn: direct play, or one codec's transcodes. */
export interface PlaybackPart {
  /** "direct", a codec, "" for an unknown codec, or "other" for the folded rest. */
  key: string;
  transcoded: boolean;
  listened: number;
  /** 0..1 of all listening. */
  share: number;
}

/**
 * Listening by how it played: direct (every codec together) first, then transcodes
 * per codec, largest first; past `maxTranscodes` codecs the rest fold into "other".
 */
export function playbackParts(rows: readonly PlaybackShare[], maxTranscodes = 3): PlaybackPart[] {
  const total = rows.reduce((s, r) => s + r.listened, 0);
  if (total <= 0) return [];
  const direct = rows.filter((r) => !r.transcoded).reduce((s, r) => s + r.listened, 0);
  const byCodec = new Map<string, number>();
  for (const r of rows) {
    if (r.transcoded) byCodec.set(r.codec, (byCodec.get(r.codec) ?? 0) + r.listened);
  }
  const transcodes = [...byCodec]
    .filter(([, s]) => s > 0)
    .sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]));
  const shown = transcodes.slice(0, maxTranscodes);
  const rest = transcodes.slice(maxTranscodes).reduce((s, [, v]) => s + v, 0);
  const parts: PlaybackPart[] = [];
  if (direct > 0) parts.push({ key: 'direct', transcoded: false, listened: direct, share: 0 });
  for (const [codec, listened] of shown)
    parts.push({ key: codec, transcoded: true, listened, share: 0 });
  if (rest > 0) parts.push({ key: 'other', transcoded: true, listened: rest, share: 0 });
  return parts.map((p) => ({ ...p, share: p.listened / total }));
}

/** -1, 0 or 1: dotted numeric versions ("1.10.0" after "1.9.2"); non-numeric parts as 0. */
export function compareVersions(a: string, b: string): number {
  const pa = a.split('.').map((x) => parseInt(x, 10) || 0);
  const pb = b.split('.').map((x) => parseInt(x, 10) || 0);
  for (let i = 0; i < Math.max(pa.length, pb.length); i++) {
    const d = (pa[i] ?? 0) - (pb[i] ?? 0);
    if (d) return Math.sign(d);
  }
  return 0;
}

/** An app build in use, and whether a newer build of the same app on the same platform is too. */
export interface ClientRow extends ClientCount {
  outdated: boolean;
}

/**
 * Apps in use, most devices first (an app that never named itself after the named
 * ones), each marked when a newer build of it is in use.
 */
export function clientRows(clients: readonly ClientCount[]): ClientRow[] {
  const newest = new Map<string, string>();
  for (const c of clients) {
    if (!c.app || !c.version) continue;
    const key = `${c.app}|${c.platform}`;
    const seen = newest.get(key);
    if (!seen || compareVersions(c.version, seen) > 0) newest.set(key, c.version);
  }
  return clients
    .map((c) => ({
      ...c,
      outdated:
        !!c.app &&
        !!c.version &&
        compareVersions(c.version, newest.get(`${c.app}|${c.platform}`)!) < 0,
    }))
    .sort(
      (a, b) =>
        b.devices - a.devices ||
        Number(!a.app) - Number(!b.app) ||
        a.app.localeCompare(b.app) ||
        compareVersions(b.version, a.version),
    );
}

/** What kind of app a client is, for its icon and label. */
export type ClientKind = 'unknown' | 'console' | 'phone' | 'web' | 'other';

export function clientKind(client: Pick<ClientInfo, 'app' | 'platform'> | null): ClientKind {
  if (!client?.app) return 'unknown';
  if (client.app === CLIENT_APP) return 'console';
  const p = client.platform.toLowerCase();
  if (p === 'ios' || p === 'android') return 'phone';
  if (p === 'web') return 'web';
  return 'other';
}

/** How a platform reads ("iOS", "Android", "Web"); anything else as sent. */
export function platformLabel(platform: string): string {
  const known: Record<string, string> = { ios: 'iOS', android: 'Android', web: 'Web' };
  return known[platform.toLowerCase()] ?? platform;
}

/**
 * The parts of a client's label: the app with its version, and the platform
 * ("AudioSilo 1.4.2", "iOS"). The console and unknown apps are named by the caller
 * (they need a translation).
 */
export function clientParts(client: Pick<ClientInfo, 'app' | 'version' | 'platform'>) {
  return {
    app: client.version ? `${client.app} ${client.version}` : client.app,
    platform: client.platform ? platformLabel(client.platform) : '',
  };
}

/** The years "Year in listening" offers: this year and the four before it, newest first. */
export function recentYears(thisYear: number, count = 5): number[] {
  return Array.from({ length: count }, (_, i) => thisYear - i);
}
