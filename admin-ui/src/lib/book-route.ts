import type { BookRef } from '@/api/types';
import type { LibrarySearch } from '@/features/library/library-search';

/**
 * Link props for a book's page (`<Link {...bookRoute(…)}>`, `navigate(bookRoute(…))`).
 * The book is addressed by its identity, library + path, never an internal id.
 */
export function bookRoute(libraryId: number, path: string) {
  return { to: '/library/book', search: { library: libraryId, path } } as const;
}

/** The book page's search params; `match` opens the match dialog (Health's "Review match"). */
export type BookSearch = { library: number; path: string; match?: true };

/** The book page's search params, or undefined when they don't name a book. */
export function parseBookSearch(s: Record<string, unknown>): BookSearch | undefined {
  const library = Number(s.library);
  const path =
    typeof s.path === 'string' ? s.path : typeof s.path === 'number' ? String(s.path) : '';
  if (!Number.isInteger(library) || library <= 0 || !path) return undefined;
  const match = s.match === true || s.match === 1 || s.match === '1';
  return match ? { library, path, match } : { library, path };
}

export const refOf = (b: { library_id: number; path: string }): BookRef => ({
  library_id: b.library_id,
  path: b.path,
});

/** A book's identity as one string, for Map keys and React keys (library + path, never an id). */
export const refKey = (b: BookRef) => `${b.library_id}\0${b.path}`;

/** The exact-value filters a name or series links to the Books list with. */
export type BooksField = 'author' | 'narrator' | 'series';

/** Whether a field (a table column's id, say) is one the Books list filters on exactly. */
export const isBooksField = (f: string): f is BooksField =>
  f === 'author' || f === 'narrator' || f === 'series';

/**
 * Link props for the Books list filtered to one author, narrator or series
 * (an exact effective value), keeping the library the current page is scoped
 * to and, from the list itself, its view. Components link with BooksLink,
 * which keeps these props stable.
 */
export function booksRoute(field: BooksField, value: string) {
  const filter: Pick<LibrarySearch, BooksField> = { [field]: value };
  return {
    to: '/library/{-$section}',
    params: { section: undefined },
    search: (prev: Pick<LibrarySearch, 'library' | 'view'>) => ({
      library: prev.library,
      view: prev.view,
      ...filter,
    }),
  } as const;
}
