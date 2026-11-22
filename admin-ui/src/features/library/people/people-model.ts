import type { MergeSuggestion, PersonCount } from '@/api/types';
import { fold } from '@/lib/utils';

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
