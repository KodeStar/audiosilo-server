import type {
  AdminBook,
  BookEditRequest,
  BookRef,
  MergeSuggestion,
  PersonCount,
} from '@/api/types';
import { refOf } from '@/lib/book-route';

// The Authors and Narrators screens' pure parts: ordering and filtering the
// people aggregate, the "N books · 12h" figures, and the bulk edits a merge (and
// its undo) send. A person is a whole field value ("Michael Kramer & Kate
// Reading" is one narrator), so a merge rewrites the field, never part of it.

export type PersonField = 'author' | 'narrator';

/** POST /admin/books/bulk takes at most this many books per request. */
export const BULK_LIMIT = 1000;

/** How many tiles render at first, and how many more each "Show more" adds. */
export const PAGE_STEP = 120;

/** Lower case without diacritics, so "Bronte" finds "Brontë". */
export function fold(s: string): string {
  return s.normalize('NFKD').replace(/\p{M}/gu, '').toLowerCase();
}

/** The people whose name contains `q` (case and accent insensitive). */
export function filterPeople(people: PersonCount[], q: string): PersonCount[] {
  const needle = fold(q.trim());
  return needle ? people.filter((p) => fold(p.name).includes(needle)) : people;
}

const byName = (a: PersonCount, b: PersonCount) => a.name.localeCompare(b.name);

/** Most books first (authors), ties by name. */
export function sortByBooks(people: PersonCount[]): PersonCount[] {
  return [...people].sort((a, b) => b.books - a.books || byName(a, b));
}

/** Most hours first (narrators), ties by name. */
export function sortByDuration(people: PersonCount[]): PersonCount[] {
  return [...people].sort((a, b) => b.duration - a.duration || byName(a, b));
}

/** A total listening time as whole hours, or minutes when under an hour. */
export function durationParts(seconds: number): { unit: 'hours' | 'minutes'; value: number } {
  const s = Math.max(0, seconds || 0);
  return s >= 3600
    ? { unit: 'hours', value: Math.round(s / 3600) }
    : { unit: 'minutes', value: Math.round(s / 60) };
}

/** The spellings a merge rewrites: every name but the suggested one. */
export function otherSpellings(s: MergeSuggestion): string[] {
  return s.names.filter((n) => n !== s.suggested);
}

/**
 * How many books carry the other spellings. The suggestion's own count covers
 * every spelling (the suggested one too), so this sums the people list; it falls
 * back to that count when the list doesn't name them.
 */
export function otherSpellingBooks(s: MergeSuggestion, people: PersonCount[]): number {
  const others = new Set(otherSpellings(s));
  const n = people.filter((p) => others.has(p.name)).reduce((sum, p) => sum + p.books, 0);
  return n || s.books;
}

/** `xs` in runs of at most `size`. */
export function chunk<T>(xs: T[], size = BULK_LIMIT): T[][] {
  const out: T[][] = [];
  for (let i = 0; i < xs.length; i += size) out.push(xs.slice(i, i + size));
  return out;
}

/** One POST /admin/books/bulk request. */
export interface BulkStep {
  books: BookRef[];
  edit: Pick<BookEditRequest, 'set' | 'revert'>;
}

/** The requests that set `field` to `suggested` on every book. */
export function mergeSteps(books: AdminBook[], field: PersonField, suggested: string): BulkStep[] {
  return chunk(books.map(refOf)).map((refs) => ({
    books: refs,
    edit: { set: { [field]: suggested } },
  }));
}

/**
 * The requests that put each merged book back. A book with no edits before the
 * merge reverts the field (back to its file tag, unlocked); an edited book gets
 * its old spelling set again, one request per spelling.
 */
export function undoSteps(books: AdminBook[], field: PersonField): BulkStep[] {
  const steps: BulkStep[] = chunk(books.filter((b) => !b.edited).map(refOf)).map((refs) => ({
    books: refs,
    edit: { revert: [field] },
  }));
  const bySpelling = new Map<string, BookRef[]>();
  for (const b of books.filter((x) => x.edited)) {
    const refs = bySpelling.get(b[field]) ?? [];
    refs.push(refOf(b));
    bySpelling.set(b[field], refs);
  }
  for (const [spelling, refs] of bySpelling) {
    for (const part of chunk(refs)) {
      steps.push({ books: part, edit: { set: { [field]: spelling } } });
    }
  }
  return steps;
}
