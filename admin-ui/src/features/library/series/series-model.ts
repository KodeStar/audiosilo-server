import type {
  AdminBook,
  MetaSeries,
  MetaSeriesOrdering,
  MetaSeriesWork,
  SeriesCount,
} from '@/api/types';
import { parseSeriesRange } from '@/features/book/field-rules';
import { refKey } from '@/lib/book-route';
import { coverModel } from '@/lib/cover-model';
import { formatDay, formatSeriesPosition } from '@/lib/format';
import { hashString } from '@/lib/monogram';
import { sameSeries, seriesKey } from '@/lib/series-key';

// The Series screen's pure parts: finding series by name or author, matching a
// book's community series rail to a local series, placing the owned books on
// it, the gaps (rail positions the server doesn't hold), "1, 2, 4-6" position
// lists and the spine row that interleaves owned books with missing entries.
//
// A position may be a range, an omnibus of books 1-3 (a book's series_index_end
// or series_list position_end; a rail entry's "1-3"): a book at a range holds
// every rail entry inside it, and a range entry is a slot of its own only on a
// rail that numbers none of the books inside it.

/** Series cards on a page. */
export const SERIES_PAGE = 24;

/** A place in a series: a position and, for a range (an omnibus, "1-3"), its end. */
export interface Span {
  position: number;
  /** A range's end, above position; absent for a single position. */
  end?: number;
}

/** One numbered entry of a community series rail. */
export interface RailEntry extends Span {
  title: string;
  /** The work's first release, YYYY[-MM[-DD]] (absent when unknown or from an older server). */
  release_date?: string;
}

/** What a rail says about a local series. */
export interface SeriesStatus {
  /** Numbered entries in the rail. */
  total: number;
  /** Rail entries the server holds, ascending. */
  have: RailEntry[];
  /** Rail entries it doesn't, ascending. */
  missing: RailEntry[];
}

/** Which community work each owned book is: work ids by refKey (resolved books only). */
export type WorkIds = ReadonlyMap<string, string>;

/** An owned book's place on the shelf. */
export interface Placed extends Span {
  book: AdminBook;
  /**
   * 'rail': it holds `position` on the rail (by its work, else by its series
   * index), and every entry up to `end` for a range. 'end': drawn after the rail,
   * holding nothing (unnumbered, or known to be a work this rail doesn't list).
   */
  slot: 'rail' | 'end';
  /** The number it shows: its rail position when placed by its work, else its series index (0 = none). */
  position: number;
}

export type Spine = ({ kind: 'book' } & Placed) | { kind: 'gap'; entry: RailEntry };

const samePosition = (a: number, b: number) => Math.abs(a - b) < 1e-6;
const atOrBefore = (a: number, b: number) => a < b || samePosition(a, b);

/** A span with its end only when it is a range. */
const span = (position: number, end = 0): Span =>
  end > position ? { position, end } : { position };

/** Whether a book placed at `p` holds rail entry `e`: the entry lies inside its span. */
const holds = (p: Span, e: Span) =>
  atOrBefore(p.position, e.position) && atOrBefore(e.end ?? e.position, p.end ?? p.position);

/**
 * The series whose name or author holds the search, compared as series keys
 * (seriesKey: "fights with" finds "He Who Fights WIth Monsters"); every series
 * for a search with no letter or number.
 */
export function filterSeries(list: SeriesCount[], q: string): SeriesCount[] {
  const needle = seriesKey(q);
  if (!needle) return list;
  return list.filter(
    (s) => seriesKey(s.name).includes(needle) || seriesKey(s.author).includes(needle),
  );
}

/**
 * A series name without a leading article or a trailing "Series", as the
 * server's match.NormalizeSeries reads one ("The Stormlight Archive" and
 * "Stormlight Archive Series" are one series).
 */
function looseSeriesKey(name: string): string {
  return seriesKey(
    name
      .trim()
      .replace(/^(?:the|a|an)\s+(?=\S)/i, '')
      .replace(/(?<=\S)\s+series$/i, ''),
  );
}

/** A rail's alternate reading order as a rail of its own (its works, its name). */
function orderingRail(o: MetaSeriesOrdering): MetaSeries {
  return {
    id: o.id,
    name: o.name,
    position: o.position ?? '',
    works: o.works,
    ordering: o.ordering,
  };
}

/**
 * The rail that is the local series `name`, else none (the shelf then goes by
 * the books' own positions, with no gaps):
 *
 * 1. the rail, or one of its reading orders, named like it (seriesKey);
 * 2. else one named like it give or take an article or a trailing "Series";
 * 3. else the rail holding the most of the series' own books by their community
 *    works (`works`), when that is at least two of them and more than half of
 *    those that resolved. A book's work sits on every series it is in, so one
 *    shared book can't tell the series apart (a Tiffany Aching novel is on the
 *    Discworld rail too); two that are most of the series are this series spelled
 *    another way. A rail named like the head of a series' name before a colon
 *    or a dash ("Discworld" for "Discworld: Tiffany Aching") is its parent, never
 *    the series ("Mistborn" for "Mistborn Trilogy" can be). Ties go to the
 *    smaller rail, the more specific series.
 */
export function pickRail(
  rails: MetaSeries[] | undefined,
  name: string,
  owned: AdminBook[] = [],
  works?: WorkIds,
): MetaSeries | undefined {
  if (!rails?.length) return undefined;
  for (const key of [seriesKey, looseSeriesKey]) {
    const want = key(name);
    if (!want) continue;
    for (const r of rails) {
      if (key(r.name) === want) return r;
      const o = r.orderings?.find((v) => key(v.name) === want);
      if (o) return orderingRail(o);
    }
  }
  const ids = new Set(owned.map((b) => works?.get(refKey(b))).filter((id) => id !== undefined));
  // "Discworld: Tiffany Aching" (or "Discworld - Witches") names a series of
  // Discworld's: its head before the separator is the parent's name.
  const head = seriesKey(name.split(/\s*:\s*|\s+[-\u2013\u2014]\s+/)[0]);
  let best: { rail: MetaSeries; held: number; size: number } | undefined;
  for (const r of rails) {
    if (head !== seriesKey(name) && seriesKey(r.name) === head) continue;
    const held = new Set(r.works.filter((w) => ids.has(w.id)).map((w) => w.id)).size;
    const size = railEntries(r).length;
    if (held < 2 || held * 2 <= ids.size) continue;
    if (!best || held > best.held || (held === best.held && size < best.size)) {
      best = { rail: r, held, size };
    }
  }
  return best?.rail;
}

/**
 * A rail work's place: a number, or a range ("1-3", an omnibus; the server's
 * metadata.ParseSeriesRange); undefined when it is unnumbered (blank, or neither).
 */
function railSpan(w: MetaSeriesWork): Span | undefined {
  if (w.position.trim() === '') return undefined;
  const n = Number(w.position);
  if (Number.isFinite(n)) return { position: n };
  const range = parseSeriesRange(w.position);
  return range && { position: range.start, end: range.end };
}

/**
 * A rail's numbered entries, ascending, one per position (the first title wins).
 * A range entry ("1-3") is one only when no single entry lies inside it: the
 * books inside it are the slots, which a book at the range holds.
 */
export function railEntries(rail: MetaSeries): RailEntry[] {
  const spans = rail.works.map((w) => ({ w, s: railSpan(w) }));
  const singles = spans.flatMap(({ s }) => (s && s.end === undefined ? [s.position] : []));
  const out: RailEntry[] = [];
  for (const { w, s } of spans) {
    if (!s || out.some((e) => samePosition(e.position, s.position))) continue;
    if (s.end !== undefined && singles.some((p) => holds(s, { position: p }))) continue;
    out.push({ ...s, title: w.title, ...(w.release_date ? { release_date: w.release_date } : {}) });
  }
  return out.sort((a, b) => a.position - b.position);
}

/** How long after its release a missing entry is still new (the server's upcoming.window_days default). */
export const NEW_RELEASE_DAYS = 90;

/** A release date's parts: year, month and day, the last two 0 when the date stops before them. */
function releaseParts(date: string): [number, number, number] | undefined {
  const m = /^(\d{4})(?:-(\d{2})(?:-(\d{2}))?)?$/.exec(date);
  if (!m) return undefined;
  const [y, mo, d] = [Number(m[1]), Number(m[2] ?? 0), Number(m[3] ?? 0)];
  if (m[2] !== undefined && (mo < 1 || mo > 12)) return undefined;
  if (m[3] !== undefined && (d < 1 || d > new Date(Date.UTC(y, mo, 0)).getUTCDate()))
    return undefined;
  return [y, mo, d];
}

/** `now`'s local calendar date as YYYY-MM-DD. */
function today(now: Date): string {
  const p = (n: number) => String(n).padStart(2, '0');
  return `${now.getFullYear()}-${p(now.getMonth() + 1)}-${p(now.getDate())}`;
}

/**
 * What a missing entry's release date makes it, as the server's upcoming job
 * reads one (compared at the date's own precision): 'preorder' when it is still
 * ahead ("2026-11" from any day of October; "2026" is not, all this year),
 * 'new' when it came out within `windowDays` of today, counted from the first
 * day of its month or year, else undefined (older, undated or unreadable).
 */
export function releaseState(
  date: string | undefined,
  now: Date,
  windowDays = NEW_RELEASE_DAYS,
): 'preorder' | 'new' | undefined {
  const parts = date ? releaseParts(date) : undefined;
  if (!date || !parts) return undefined;
  const day = today(now);
  if (date > day.slice(0, date.length)) return 'preorder';
  const [y, mo, d] = parts;
  const start = Date.UTC(y, Math.max(mo, 1) - 1, Math.max(d, 1));
  const [ty, tm, td] = day.split('-').map(Number);
  const from = Date.UTC(ty, tm - 1, td) - windowDays * 86_400_000;
  return start >= from ? 'new' : undefined;
}

/**
 * A release date in words at its own precision: "12 Nov 2026", "Nov 2026" or
 * "2026"; '' for one that isn't a date. The calendar date as written, never
 * moved by the viewer's time zone.
 */
export function formatReleaseDate(date: string | undefined, lang: string): string {
  const parts = date ? releaseParts(date) : undefined;
  if (!parts) return '';
  const [y, mo, d] = parts;
  if (!mo) return String(y);
  const p = (n: number) => String(n).padStart(2, '0');
  return formatDay(`${y}-${p(mo)}-${p(d || 1)}`, lang, {
    ...(d ? { day: 'numeric' } : {}),
    month: 'short',
    year: 'numeric',
  });
}

/**
 * A book's place in the named series (its main series' without a name): a book
 * in several series has its own position in each (series_list), a range with its
 * end. The series is matched as the server groups its spellings (sameSeries).
 */
export function spanIn(book: AdminBook, series?: string): Span {
  if (series === undefined) return span(book.series_index, book.series_index_end);
  const s = book.series_list.find((r) => sameSeries(r.name, series));
  return span(s?.position ?? 0, s?.position_end);
}

/**
 * The numbering a file's title often carries before the title, as the server's
 * match cuts it: "01 - ", "7. ", "Book 3: ", "#4 "; a short code glued to its
 * number, "BV01 - ", "SW06: "; the code spaced from it, "HALO 28 - "; an
 * upper-case code with only a space, "DF15.5 ".
 */
const LEAD_NUMBERING = [
  /^\s*(?:(?:book|vol(?:ume)?\.?|part|band|teil|tome)\s*)?#?\d{1,3}(?:\.\d+)?(?:\s*[-.:_)\]]\s*|\s+)/i,
  /^\s*[a-z]{1,6}\d{1,3}(?:\.\d+)?\s*[-.:_)\]]\s+/i,
  /^\s*[a-z]{1,6}\s\d{1,3}(?:\.\d+)?\s*[-._)\]]\s+/i,
  /^\s*[A-Z]{1,6}\d{1,3}(?:\.\d+)?\s+/,
];

/**
 * The forms a title is compared in: without a leading numbering, with and
 * without a trailing bracketed part ("We Are Legion (We Are Bob)"), no case,
 * accents or punctuation.
 */
function titleKeys(title: string): string[] {
  let bare = title.trim();
  for (const re of LEAD_NUMBERING) {
    const rest = bare.replace(re, '').trim();
    if (rest !== bare) {
      if (rest) bare = rest;
      break;
    }
  }
  const short = bare.replace(/\s*[([][^()[\]]*[)\]]\s*$/, '');
  return [seriesKey(bare), seriesKey(short)].filter(Boolean);
}

/** Whether two titles name one book (titleKeys share a form). */
export function sameTitle(a: string, b: string): boolean {
  const keys = titleKeys(a);
  return titleKeys(b).some((k) => keys.includes(k));
}

/**
 * Where each owned book sits on the shelf of `series` (the books' main one when
 * absent). A book whose community work is on the rail holds that work's position
 * (a range: every entry inside it), whatever its own position says (local
 * numbering is often missing or different). A book known to be another work holds
 * none and goes to the end; one that didn't resolve (no identifier, no match, or
 * no rail yet) falls back to its position in the series (a range, an omnibus of
 * 1-3, holds 1, 2 and 3). Without one, it holds the rail entry its title is
 * (sameTitle: "BV03 - All These Worlds" is All These Worlds), when no other book
 * holds that entry, and else goes to the end.
 */
export function placeBooks(
  owned: AdminBook[],
  rail?: MetaSeries,
  works?: WorkIds,
  series?: string,
): Placed[] {
  const byIndex = (book: AdminBook): Placed => {
    const at = spanIn(book, series);
    return { book, slot: at.position > 0 ? 'rail' : 'end', ...at };
  };
  if (!rail) return owned.map(byIndex);
  // Each work's place on the rail: its first numbered one.
  const spans = new Map<string, Span>();
  for (const w of rail.works) {
    const at = railSpan(w);
    if (at && !spans.has(w.id)) spans.set(w.id, at);
  }
  const placed = owned.map((book): Placed => {
    const work = works?.get(refKey(book));
    if (work === undefined) return byIndex(book);
    const at = spans.get(work);
    return at === undefined
      ? { book, slot: 'end', ...spanIn(book, series) }
      : { book, slot: 'rail', ...at };
  });
  const held = placed.filter((p) => p.slot === 'rail');
  const free = railEntries(rail)
    .filter((e) => !held.some((p) => holds(p, e)))
    .map((entry) => ({ entry, keys: titleKeys(entry.title) }));
  return placed.map((p) => {
    if (p.slot === 'rail' || works?.has(refKey(p.book))) return p;
    const keys = titleKeys(p.book.title);
    const at = free.findIndex((f) => f.keys.some((k) => keys.includes(k)));
    if (at < 0) return p;
    const [{ entry }] = free.splice(at, 1);
    return { book: p.book, slot: 'rail', ...span(entry.position, entry.end) };
  });
}

/**
 * The rail against the books the server holds, placed by placeBooks: an entry is
 * held when a book on the rail holds it (a book at a range, every entry inside it).
 */
export function seriesStatus(rail: MetaSeries, placed: Placed[]): SeriesStatus {
  const held = placed.filter((p) => p.slot === 'rail');
  const entries = railEntries(rail);
  const isHeld = (e: RailEntry) => held.some((p) => holds(p, e));
  return {
    total: entries.length,
    have: entries.filter(isHeld),
    missing: entries.filter((e) => !isHeld(e)),
  };
}

/** A place as it is written: "2", or "1-3" for a range. */
export function formatSpan(s: Span, fmt: (n: number) => string = String): string {
  return formatSeriesPosition(s.position, s.end, fmt);
}

/**
 * Positions as a short list: a run of three or more whole numbers collapses
 * to "3-5" ("1, 2, 4-6, 7.5"); a range is written as one ("1-3").
 */
export function formatPositions(ns: Span[], fmt: (n: number) => string = String): string {
  const whole = (s: Span) => s.end === undefined && Number.isInteger(s.position);
  const parts: string[] = [];
  let i = 0;
  while (i < ns.length) {
    let j = i;
    while (
      j + 1 < ns.length &&
      whole(ns[j]) &&
      whole(ns[j + 1]) &&
      ns[j + 1].position === ns[j].position + 1
    ) {
      j++;
    }
    if (j - i >= 2) {
      parts.push(`${fmt(ns[i].position)}-${fmt(ns[j].position)}`);
    } else {
      for (let k = i; k <= j; k++) parts.push(formatSpan(ns[k], fmt));
    }
    i = j + 1;
  }
  return parts.join(', ');
}

/**
 * The shelf: placed books and missing entries in series order, then the books
 * at the end (numbered ones by their number, unnumbered last).
 */
export function spineRow(placed: Placed[], missing: RailEntry[]): Spine[] {
  const spines: Spine[] = [
    ...placed.map((p): Spine => ({ kind: 'book', ...p })),
    ...missing.map((entry): Spine => ({ kind: 'gap', entry })),
  ];
  const order = (s: Spine): [number, number] => {
    if (s.kind === 'gap') return [s.entry.position, 0];
    if (s.slot === 'rail') return [s.position, 0];
    return [Infinity, s.position > 0 ? s.position : Infinity];
  };
  const cmp = (x: number, y: number) => (x === y ? 0 : x < y ? -1 : 1);
  const title = (s: Spine) => (s.kind === 'book' ? s.book.title : s.entry.title);
  return spines.sort((a, b) => {
    const [a1, a2] = order(a);
    const [b1, b2] = order(b);
    return cmp(a1, b1) || cmp(a2, b2) || title(a).localeCompare(title(b));
  });
}

/** The book whose community metadata names the series: the first one matched. */
export function metaCandidate(owned: AdminBook[]): AdminBook | undefined {
  return owned.find((b) => b.matched);
}

/** Book lengths, in hours, at the shortest and tallest spines. */
const SHORT_HOURS = 3;
const LONG_HOURS = 30;

/**
 * A spine's height as a share of the shelf. A book's length sets it, on a log scale
 * from 72% (3 h or less) to 98% (30 h or more), so a long book stands taller than a
 * short one on every shelf. Without a length (`seconds` 0, or a missing entry), it
 * is varied but stable per key (80-95%).
 */
export function spineHeight(seconds: number, key: string): number {
  if (!(seconds > 0)) return 80 + (hashString(key) % 16);
  const t = Math.log(seconds / 3600 / SHORT_HOURS) / Math.log(LONG_HOURS / SHORT_HOURS);
  return Math.round(72 + 26 * Math.min(1, Math.max(0, t)));
}

/** A spine's colours: its body, its two bands and its type. */
export interface SpineColors {
  body: string;
  band: string;
  ink: string;
}

const INK = '#121c36';
const WHITE = '#ffffff';

/** WCAG relative luminance of `#rrggbb`. */
function luminance(hex: string): number {
  const n = parseInt(hex.slice(1), 16);
  const lin = (c: number) => {
    const v = c / 255;
    return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4;
  };
  return 0.2126 * lin((n >> 16) & 255) + 0.7152 * lin((n >> 8) & 255) + 0.0722 * lin(n & 255);
}

/** WCAG contrast ratio between two `#rrggbb` colours. */
function contrast(a: string, b: string): number {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x);
  return (hi + 0.05) / (lo + 0.05);
}

/**
 * A spine in its cover's colours, as the player's shelf draws one (its
 * `spinePalette`, components/series/spine-colors.ts): the body is the cover's
 * dominant colour, the bands its vibrant one (the server only sends one that reads
 * on the body), else the type colour, and the type whichever of ink or white reads
 * best on the body. A book whose cover colour the server hasn't read takes its
 * procedural cover's palette instead, so it still keeps its colours between visits.
 */
export function spineColors(b: Pick<AdminBook, 'title' | 'author' | 'cover_color'>): SpineColors {
  const cc = b.cover_color;
  if (!cc) {
    const [body, band, , ink] = coverModel(b.title, b.author).palette;
    return { body, band, ink };
  }
  const ink = contrast(cc.bg, WHITE) >= contrast(cc.bg, INK) ? WHITE : INK;
  return { body: cc.bg, band: cc.accent ?? ink, ink };
}
