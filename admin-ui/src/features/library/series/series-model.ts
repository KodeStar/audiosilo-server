import type {
  AdminBook,
  AdminBookPage,
  MetaSeries,
  MetaSeriesOrdering,
  MetaSeriesWork,
} from '@/api/types';
import { refKey } from '@/lib/book-route';
import { coverModel } from '@/lib/cover-model';
import { hashString } from '@/lib/monogram';

// The Series screen's pure parts: grouping the series-sorted book list into
// series, matching a book's community series rail to a local series, placing
// the owned books on it, the gaps (rail positions the server doesn't hold),
// "1, 2, 4-6" position lists and the spine row that interleaves owned books
// with missing entries.

/** Series a page of cards shows at first, and how many more each "Show more" adds. */
export const CARD_STEP = 24;

/** One numbered entry of a community series rail. */
export interface RailEntry {
  position: number;
  title: string;
}

/** What a rail says about a local series. */
export interface SeriesStatus {
  /** Numbered entries in the rail. */
  total: number;
  /** Rail positions the server holds, ascending. */
  have: number[];
  /** Rail entries it doesn't, ascending. */
  missing: RailEntry[];
}

/** Which community work each owned book is: work ids by refKey (resolved books only). */
export type WorkIds = ReadonlyMap<string, string>;

/** An owned book's place on the shelf. */
export interface Placed {
  book: AdminBook;
  /**
   * 'rail': it holds `position` on the rail (by its work, else by its series
   * index). 'end': drawn after the rail, holding nothing (unnumbered, or known to
   * be a work this rail doesn't list).
   */
  slot: 'rail' | 'end';
  /** The number it shows: its rail position when placed by its work, else its series index (0 = none). */
  position: number;
}

export type Spine = ({ kind: 'book' } & Placed) | { kind: 'gap'; entry: RailEntry };

const samePosition = (a: number, b: number) => Math.abs(a - b) < 1e-6;

/** A series name for comparison: no case, accents, punctuation or spacing. */
export function seriesKey(name: string): string {
  return name
    .normalize('NFKD')
    .replace(/[^\p{L}\p{N}]/gu, '')
    .toLowerCase();
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
 *    another way. A rail whose name the series' name extends ("Discworld" for
 *    "Discworld: Tiffany Aching") is the parent, never the series. Ties go to the
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
  const local = seriesKey(name);
  let best: { rail: MetaSeries; held: number; size: number } | undefined;
  for (const r of rails) {
    const parent = seriesKey(r.name);
    if (parent && local.startsWith(parent)) continue;
    const held = new Set(r.works.filter((w) => ids.has(w.id)).map((w) => w.id)).size;
    const size = railEntries(r).length;
    if (held < 2 || held * 2 <= ids.size) continue;
    if (!best || held > best.held || (held === best.held && size < best.size)) {
      best = { rail: r, held, size };
    }
  }
  return best?.rail;
}

/** A rail work's position, NaN when it is unnumbered (blank, or not a number). */
const railPosition = (w: MetaSeriesWork) => (w.position.trim() === '' ? NaN : Number(w.position));

/** A rail's numbered entries, ascending, one per position (the first title wins). */
export function railEntries(rail: MetaSeries): RailEntry[] {
  const out: RailEntry[] = [];
  for (const w of rail.works) {
    const position = railPosition(w);
    if (!Number.isFinite(position) || out.some((e) => samePosition(e.position, position))) continue;
    out.push({ position, title: w.title });
  }
  return out.sort((a, b) => a.position - b.position);
}

/**
 * A book's position in the named series (its main series' index without a name):
 * a book in several series has its own position in each (series_list).
 */
export function positionIn(book: AdminBook, series?: string): number {
  if (series === undefined) return book.series_index;
  return book.series_list.find((s) => s.name === series)?.position ?? 0;
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
 * absent). A book whose community work is on the rail holds that work's position,
 * whatever its own position says (local numbering is often missing or
 * different). A book known to be another work holds none and goes to the end;
 * one that didn't resolve (no identifier, no match, or no rail yet) falls back to
 * its position in the series. Without one, it holds the rail entry its title is
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
    const position = positionIn(book, series);
    return { book, slot: position > 0 ? 'rail' : 'end', position };
  };
  if (!rail) return owned.map(byIndex);
  // Each work's position on the rail: its first numbered one.
  const positions = new Map<string, number>();
  for (const w of rail.works) {
    const position = railPosition(w);
    if (Number.isFinite(position) && !positions.has(w.id)) positions.set(w.id, position);
  }
  const placed = owned.map((book): Placed => {
    const work = works?.get(refKey(book));
    if (work === undefined) return byIndex(book);
    const position = positions.get(work);
    return position === undefined
      ? { book, slot: 'end', position: positionIn(book, series) }
      : { book, slot: 'rail', position };
  });
  const held = placed.filter((p) => p.slot === 'rail').map((p) => p.position);
  const free = railEntries(rail)
    .filter((e) => !held.some((i) => samePosition(i, e.position)))
    .map((entry) => ({ entry, keys: titleKeys(entry.title) }));
  return placed.map((p) => {
    if (p.slot === 'rail' || works?.has(refKey(p.book))) return p;
    const keys = titleKeys(p.book.title);
    const at = free.findIndex((f) => f.keys.some((k) => keys.includes(k)));
    if (at < 0) return p;
    const [{ entry }] = free.splice(at, 1);
    return { ...p, slot: 'rail', position: entry.position };
  });
}

/** The rail against the books the server holds, placed by placeBooks. */
export function seriesStatus(rail: MetaSeries, placed: Placed[]): SeriesStatus {
  const held = placed.filter((p) => p.slot === 'rail').map((p) => p.position);
  const entries = railEntries(rail);
  const isHeld = (e: RailEntry) => held.some((i) => samePosition(i, e.position));
  return {
    total: entries.length,
    have: entries.filter(isHeld).map((e) => e.position),
    missing: entries.filter((e) => !isHeld(e)),
  };
}

/**
 * Positions as a short list: a run of three or more whole numbers collapses
 * to "3-5" ("1, 2, 4-6, 7.5").
 */
export function formatPositions(ns: number[], fmt: (n: number) => string = String): string {
  const parts: string[] = [];
  let i = 0;
  while (i < ns.length) {
    let j = i;
    while (
      j + 1 < ns.length &&
      Number.isInteger(ns[j]) &&
      Number.isInteger(ns[j + 1]) &&
      ns[j + 1] === ns[j] + 1
    ) {
      j++;
    }
    if (j - i >= 2) {
      parts.push(`${fmt(ns[i])}-${fmt(ns[j])}`);
    } else {
      for (let k = i; k <= j; k++) parts.push(fmt(ns[k]));
    }
    i = j + 1;
  }
  return parts.join(', ');
}

/** Owned books by series index (unnumbered last), then title. */
export function sortSeriesBooks(books: AdminBook[]): AdminBook[] {
  const idx = (b: AdminBook) => (b.series_index > 0 ? b.series_index : Infinity);
  return [...books].sort((a, b) => idx(a) - idx(b) || a.title.localeCompare(b.title));
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

/**
 * The series-sorted book list grouped by series name. Series books come first
 * and books without a series last, so `done` turns true on the first page
 * holding a book without one (or when there is no next page).
 */
export function groupSeriesPages(
  pages: AdminBookPage[],
  hasNextPage: boolean,
): { bySeries: Map<string, AdminBook[]>; done: boolean } {
  const bySeries = new Map<string, AdminBook[]>();
  let done = !hasNextPage;
  for (const page of pages) {
    for (const b of page.books) {
      if (!b.series) {
        done = true;
        continue;
      }
      const list = bySeries.get(b.series) ?? [];
      list.push(b);
      bySeries.set(b.series, list);
    }
  }
  for (const [name, list] of bySeries) bySeries.set(name, sortSeriesBooks(list));
  return { bySeries, done };
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
