import { BULK_LIMIT } from '@/api/client';
import type {
  AdminBook,
  BookEditRequest,
  BookRef,
  MergeSuggestion,
  PersonCount,
  PersonField,
} from '@/api/types';
import { refOf } from '@/lib/book-route';
import { chunk, fold } from '@/lib/utils';

// The Authors and Narrators screens' pure parts: ordering and filtering the
// people aggregate, and the bulk edits a merge (and its undo) send. A person is
// a whole field value ("Michael Kramer & Kate Reading" is one narrator), so a
// merge rewrites the field, never part of it.

/** How many tiles render at first, and how many more each "Show more" adds. */
export const PAGE_STEP = 120;

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

/** One POST /admin/books/bulk request. */
export interface BulkStep {
  books: BookRef[];
  edit: Pick<BookEditRequest, 'set' | 'revert'>;
}

/** The requests that set `field` to `suggested` on every book. */
export function mergeSteps(books: AdminBook[], field: PersonField, suggested: string): BulkStep[] {
  return chunk(books.map(refOf), BULK_LIMIT).map((refs) => ({
    books: refs,
    edit: { set: { [field]: suggested } },
  }));
}

/**
 * The requests that put each merged book back, from the books as they were before
 * the merge. A book whose field had no override reverts it (back to what the scan
 * found, unlocked); one whose field had an override gets its old spelling set
 * again, one request per spelling. Edits to other fields don't matter.
 */
export function undoSteps(books: AdminBook[], field: PersonField): BulkStep[] {
  const overridden = (b: AdminBook) => b.edited_fields.includes(field);
  const steps: BulkStep[] = chunk(books.filter((b) => !overridden(b)).map(refOf), BULK_LIMIT).map(
    (refs) => ({ books: refs, edit: { revert: [field] } }),
  );
  const bySpelling = new Map<string, BookRef[]>();
  for (const b of books.filter(overridden)) {
    const refs = bySpelling.get(b[field]) ?? [];
    refs.push(refOf(b));
    bySpelling.set(b[field], refs);
  }
  for (const [spelling, refs] of bySpelling) {
    for (const part of chunk(refs, BULK_LIMIT)) {
      steps.push({ books: part, edit: { set: { [field]: spelling } } });
    }
  }
  return steps;
}
