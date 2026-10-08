import type { SeriesRef } from '@/api/types';
import { CONTROL, MAX_SERIES_INDEX, MAX_SHORT, NUMBER } from './field-rules';

// A book's other series (the more_series field, catalog/moreseries.go): stored
// as canonical JSON, [{"name": ..., "position": ...}] ("" for none), edited and
// shown as one line, "Discworld: City Watch #1; Omnibus" (a position after "#",
// entries apart by ";", so a name may hold a comma).

const MAX_ENTRIES = 20;
const ENTRY = /^(.*?)(?:\s*#\s*([^#\s]+))?$/;

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
    .map((s) => (s.position > 0 ? `${s.name} #${s.position}` : s.name))
    .join('; ');
}

/**
 * The canonical value of an edited line (or of a stored value, which a revert's
 * undo sends back), with what's wrong with it: mirrors catalog.normalizeMoreSeries
 * (names trimmed, blanks dropped, each name once, positions 0 to 100000).
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

/**
 * A value in the stored form (a JSON list of {name, position}), else undefined: a
 * typed line that only starts with "[" ("[Untitled] #2") is read as a line.
 */
function storedRefs(v: string): { name: string; position: string | number }[] | undefined {
  if (!v.startsWith('[')) return undefined;
  try {
    const list: unknown = JSON.parse(v);
    const entry = (e: unknown) =>
      typeof e === 'object' && e !== null && typeof (e as { name?: unknown }).name === 'string';
    return Array.isArray(list) && list.every(entry)
      ? (list as { name: string; position: string | number }[])
      : undefined;
  } catch {
    return undefined;
  }
}

function checkRefs(entries: { name: string; position: string | number }[]): {
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
    // As series_index: a plain number (none when blank), 0 to 100000.
    const raw = String(e.position ?? '').trim();
    const position = raw ? Number(raw) : 0;
    if ((raw && !NUMBER.test(raw)) || position < 0 || position > MAX_SERIES_INDEX) {
      return { value: '', error: 'book.invalid.moreSeries' };
    }
    out.push({ name, position });
  }
  if (out.length > MAX_ENTRIES) return { value: '', error: 'book.invalid.moreSeries' };
  return { value: encode(out) };
}
