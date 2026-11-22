import { BULK_LIMIT } from '@/api/client';
import type {
  MergedBook,
  MergeField,
  BookEditRequest,
  BookRef,
  MergeSuggestion,
  PersonCount,
} from '@/api/types';
import { refOf } from '@/lib/book-route';
import { chunk, fold } from '@/lib/utils';

/** How many tiles a page of Authors or Narrators holds. */
export const PEOPLE_PAGE = 120;

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

/** Undo groups books with the same prior override into ordinary bulk edits. */
export function undoSteps(books: MergedBook[], field: MergeField) {
  const groups = new Map<string, { books: BookRef[]; edit: BookEditRequest }>();
  for (const b of books) {
    const key = JSON.stringify([b.source, b.previous]);
    let group = groups.get(key);
    if (!group) {
      const edit =
        b.previous === null
          ? { revert: [field] }
          : { set: { [field]: b.previous }, source: b.source };
      group = { books: [], edit };
      groups.set(key, group);
    }
    group.books.push(refOf(b));
  }
  return [...groups.values()].flatMap((g) =>
    chunk(g.books, BULK_LIMIT).map((refs) => ({ books: refs, edit: g.edit })),
  );
}
