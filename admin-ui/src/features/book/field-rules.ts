// The limits catalog.normalizeOverride enforces, shared by the field checks
// (book-model.ts) and the other-series line (more-series.ts).

export const MAX_SHORT = 500;
export const MAX_SERIES_INDEX = 100000;
// unicode.IsControl: C0, DEL and C1.
// eslint-disable-next-line no-control-regex
export const CONTROL = /[\u0000-\u001f\u007f-\u009f]/;
export const NUMBER = /^[+-]?(\d+\.?\d*|\.\d+)([eE][+-]?\d+)?$/;
/** The widest range a series position may be (metadata.MaxSeriesSpan). */
export const MAX_SERIES_SPAN = 100;

/**
 * A series position that is a range, an omnibus of books 1 to 3: two numbers
 * apart by a hyphen or an en dash, spaces around it allowed ("1-3", "1 - 3",
 * "1–3"), with 0 < start < end <= 100000 and at most MAX_SERIES_SPAN apart.
 * Mirrors metadata.ParseSeriesRange; undefined for anything else (a single
 * number included).
 */
export function parseSeriesRange(raw: string): { start: number; end: number } | undefined {
  const v = raw.trim();
  for (let i = 1; i < v.length; i++) {
    if (v[i] !== '-' && v[i] !== '–') continue;
    const [a, b] = [v.slice(0, i).trim(), v.slice(i + 1).trim()];
    if (!NUMBER.test(a) || !NUMBER.test(b)) continue; // "1e-3-4": a dash inside a number
    const [start, end] = [Number(a), Number(b)];
    return validSeriesRange(start, end) ? { start, end } : undefined;
  }
  return undefined;
}

/**
 * Whether start-end is a range a series position may hold: 0 < start < end <=
 * 100000, at most MAX_SERIES_SPAN apart (metadata.ValidSeriesRange).
 */
export function validSeriesRange(start: number, end: number): boolean {
  return start > 0 && end > start && end <= MAX_SERIES_INDEX && end - start <= MAX_SERIES_SPAN;
}
