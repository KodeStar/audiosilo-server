import type { AdminBook, AdminBookPage, MetaSeries } from '@/api/types';
import { hashString } from '@/lib/monogram';

// The Series screen's pure parts: grouping the series-sorted book list into
// series, matching a book's community series rail to a local series, the gaps
// (rail positions the server doesn't hold), "1, 2, 4-6" position lists and the
// spine row that interleaves owned books with missing entries.

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

export type Spine =
  { kind: 'book'; book: AdminBook; position: number } | { kind: 'gap'; entry: RailEntry };

const samePosition = (a: number, b: number) => Math.abs(a - b) < 1e-6;

/** A series name for comparison: no case, accents, punctuation or spacing. */
export function seriesKey(name: string): string {
  return name
    .normalize('NFKD')
    .replace(/[^\p{L}\p{N}]/gu, '')
    .toLowerCase();
}

/** The rail named like the local series, else the book's first rail. */
export function pickRail(rails: MetaSeries[] | undefined, name: string): MetaSeries | undefined {
  if (!rails?.length) return undefined;
  const key = seriesKey(name);
  return rails.find((r) => seriesKey(r.name) === key) ?? rails[0];
}

/** A rail's numbered entries, ascending, one per position (the first title wins). */
export function railEntries(rail: MetaSeries): RailEntry[] {
  const out: RailEntry[] = [];
  for (const w of rail.works) {
    const position = w.position.trim() === '' ? NaN : Number(w.position);
    if (!Number.isFinite(position) || out.some((e) => samePosition(e.position, position))) continue;
    out.push({ position, title: w.title });
  }
  return out.sort((a, b) => a.position - b.position);
}

/**
 * The rail against the books the server holds (compared by series_index; a
 * book without one, index 0, holds no position).
 */
export function seriesStatus(rail: MetaSeries, owned: AdminBook[]): SeriesStatus {
  const held = owned.map((b) => b.series_index).filter((i) => i > 0);
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
 * The shelf: owned books and missing entries in series order, unnumbered books
 * at the end.
 */
export function spineRow(owned: AdminBook[], missing: RailEntry[]): Spine[] {
  const spines: Spine[] = [
    ...owned.map((book): Spine => ({ kind: 'book', book, position: book.series_index })),
    ...missing.map((entry): Spine => ({ kind: 'gap', entry })),
  ];
  const pos = (s: Spine) => {
    const p = s.kind === 'book' ? s.position : s.entry.position;
    return s.kind === 'book' && p <= 0 ? Infinity : p;
  };
  return spines.sort((a, b) => {
    const d = pos(a) - pos(b);
    if (d) return d;
    const title = (s: Spine) => (s.kind === 'book' ? s.book.title : s.entry.title);
    return title(a).localeCompare(title(b));
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

/** The book whose community metadata names the series: the first with an ASIN or ISBN. */
export function metaCandidate(owned: AdminBook[]): AdminBook | undefined {
  return owned.find((b) => b.asin || b.isbn);
}

/** A spine's height as a share of the shelf (80-95%), varied but stable per title. */
export function spineHeight(key: string): number {
  return 80 + (hashString(key) % 16);
}
