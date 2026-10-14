import type { SeriesRef } from '@/api/types';
import { formatSeriesPosition } from '@/lib/format';
import {
  CONTROL,
  MAX_SERIES_INDEX,
  MAX_SHORT,
  NUMBER,
  parseSeriesRange,
  validSeriesRange,
} from './field-rules';

// A book's other series (the more_series field, catalog/moreseries.go): stored
// as canonical JSON, [{"name": ..., "position": ..., "position_end": ...}] ("" for
// none; position_end only for a range, an omnibus of books 1-3), edited and
// shown as one line, "Discworld: City Watch #1; Omnibus #1-3" (a position after
// "#", entries apart by ";", so a name may hold a comma).

const MAX_ENTRIES = 20;
// The position: a number, or a range ("1-3", "1 - 3", "1–3").
const ENTRY = /^(.*?)(?:\s*#\s*([^#\s]+(?:\s*[-\u2013]\s*[^#\s]+)?))?$/;

/**
 * A position as the series_index field stores it and the line shows it: "2", "1-3"
 * for a range, "" for none (catalog.FormatSeriesRange).
 */
export function positionText(ref: Pick<SeriesRef, 'position' | 'position_end'>): string {
  return ref.position > 0 ? formatSeriesPosition(ref.position, ref.position_end) : '';
}

/** The series `name` at a stored series_index value ("2", "1-3", "" for none). */
export function seriesRef(name: string, stored: string): SeriesRef {
  const range = parseSeriesRange(stored);
  if (range) return { name, position: range.start, position_end: range.end };
  return { name, position: Number(stored) || 0 };
}

/** The stored list (canonical JSON, "" or "[]" for none). */
export function moreSeriesRefs(stored: string): SeriesRef[] {
  if (!stored) return [];
  try {
    const v: unknown = JSON.parse(stored);
    return Array.isArray(v) ? (v as SeriesRef[]) : [];
  } catch {
    return [];
  }
}

/** The canonical value of a list, as the server stores it ("" for none). */
const encode = (refs: SeriesRef[]) => (refs.length ? JSON.stringify(refs) : '');

/** A list as the one line it is edited and shown as. */
export function moreSeriesText(stored: string): string {
  return moreSeriesRefs(stored)
    .map((s) => (s.position > 0 ? `${s.name} #${positionText(s)}` : s.name))
    .join('; ');
}

/**
 * The canonical value of an edited line (or of a stored value, which a revert's
 * undo sends back), with what's wrong with it: mirrors catalog.normalizeMoreSeries
 * (names trimmed, blanks dropped, each name once, positions 0 to 100000, a range
 * as the series_index field takes one).
 */
export function parseMoreSeries(raw: string): { value: string; error?: string } {
  const v = raw.trim();
  const stored = storedRefs(v);
  if (stored) return checkRefs(stored);
  return checkRefs(
    v
      .split(';')
      .map((part) => part.trim())
      .filter(Boolean)
      .map((part) => {
        const [, name = '', position = ''] = ENTRY.exec(part) ?? [];
        return { name, position };
      }),
  );
}

/** An entry as read: a line's position text, or a stored entry's numbers. */
interface RawEntry {
  name: string;
  position: string | number;
  position_end?: unknown;
}

/**
 * A value in the stored form (a JSON list of {name, position[, position_end]}),
 * else undefined: a typed line that only starts with "[" ("[Untitled] #2") is read
 * as a line.
 */
function storedRefs(v: string): RawEntry[] | undefined {
  if (!v.startsWith('[')) return undefined;
  try {
    const list: unknown = JSON.parse(v);
    const entry = (e: unknown) =>
      typeof e === 'object' && e !== null && typeof (e as { name?: unknown }).name === 'string';
    return Array.isArray(list) && list.every(entry) ? (list as RawEntry[]) : undefined;
  } catch {
    return undefined;
  }
}

function checkRefs(entries: RawEntry[]): {
  value: string;
  error?: string;
} {
  const out: SeriesRef[] = [];
  const seen = new Set<string>();
  for (const e of entries) {
    const name = e.name.trim();
    if ([...name].length > MAX_SHORT) return { value: '', error: 'book.invalid.tooLong' };
    if (CONTROL.test(name)) return { value: '', error: 'book.invalid.control' };
    if (!name || seen.has(name)) continue;
    seen.add(name);
    // As series_index: a plain number (none when blank), 0 to 100000, or a range
    // (a line's "1-3", a stored entry's position_end).
    const raw = String(e.position ?? '').trim();
    const end = e.position_end ?? 0;
    if (typeof end !== 'number') return { value: '', error: 'book.invalid.moreSeries' };
    const position = raw ? Number(raw) : 0;
    if (end !== 0 && typeof e.position !== 'string') {
      // A stored entry's range.
      if (!validSeriesRange(position, end)) return { value: '', error: 'book.invalid.moreSeries' };
      out.push({ name, position, position_end: end });
      continue;
    }
    const range = parseSeriesRange(raw); // a line's "1-3"
    if (range) {
      out.push({ name, position: range.start, position_end: range.end });
      continue;
    }
    if ((raw && !NUMBER.test(raw)) || position < 0 || position > MAX_SERIES_INDEX) {
      return { value: '', error: 'book.invalid.moreSeries' };
    }
    out.push({ name, position });
  }
  if (out.length > MAX_ENTRIES) return { value: '', error: 'book.invalid.moreSeries' };
  return { value: encode(out) };
}
