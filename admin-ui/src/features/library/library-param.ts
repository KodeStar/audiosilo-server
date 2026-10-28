import { useCallback, useMemo } from 'react';
import { useNavigate, useSearch } from '@tanstack/react-router';
import { useBookLibraries, useLibraries } from '@/api/hooks';
import type { AdminLibrary } from '@/api/types';
import { isBookLibrary } from '@/lib/library-kind';
import { compact } from '@/lib/utils';
import type { LibrarySearch, Update } from './library-search';

/**
 * Writes the Library screens' search params from the previous ones, dropping
 * what the change unsets, in place (a filter isn't a history entry) and where
 * the page is scrolled (a filter isn't a new page either).
 */
export function useUpdateSearch(): Update {
  const navigate = useNavigate();
  return useCallback(
    (fn) =>
      void navigate({
        to: '.',
        search: (prev: LibrarySearch) => compact(fn(prev)),
        replace: true,
        resetScroll: false,
      }),
    [navigate],
  );
}

/**
 * Whether the Library screens offer a library choice: only from two book
 * libraries, since one choice filters nothing (LibraryFilter and the books
 * filter sheet's Library group share this rule).
 */
export function offersLibraryChoice(bookLibraries: readonly unknown[]): boolean {
  return bookLibraries.length >= 2;
}

/**
 * `?library=` as the Library screens take it: a podcast library's id (an old
 * link, or one typed) reads as all libraries, so the page doesn't sit on an
 * empty list. Until the libraries load, the id stands.
 */
export function bookLibraryOf(
  id: number | undefined,
  libraries: readonly Pick<AdminLibrary, 'id' | 'kind'>[] | undefined,
): number | undefined {
  const l = id === undefined ? undefined : libraries?.find((x) => x.id === id);
  return l && !isBookLibrary(l) ? undefined : id;
}

/** The Library screens' search params, with `library` as bookLibraryOf takes it. */
export function useLibrarySearch(): LibrarySearch {
  const search = useSearch({ strict: false }) as LibrarySearch;
  const library = bookLibraryOf(search.library, useLibraries().data);
  return useMemo(
    () => (library === search.library ? search : { ...search, library }),
    [search, library],
  );
}

/**
 * The Library screens' `?library=` filter: one book library's id, or undefined
 * for all. Another library starts its list on the first page.
 */
export function useLibraryParam(): [number | undefined, (id: number | undefined) => void] {
  const { library } = useLibrarySearch();
  const update = useUpdateSearch();
  const set = useCallback(
    (id: number | undefined) => update((prev) => ({ ...prev, library: id, page: undefined })),
    [update],
  );
  return [library, set];
}

/**
 * A Library page of cards found by a search (Series, Authors, Narrators): the
 * search and the page, both in the URL so Back returns to them. A new search
 * replaces its history entry and starts again on the first page; a new page is a
 * history entry (Back returns to the page before) and opens at the top.
 */
export function useSearchPage() {
  const { q = '', page } = useSearch({ strict: false }) as LibrarySearch;
  const update = useUpdateSearch();
  const navigate = useNavigate();
  const search = useCallback(
    (value: string) => update((prev) => ({ ...prev, q: value, page: undefined })),
    [update],
  );
  const goTo = useCallback(
    (n: number) =>
      void navigate({
        to: '.',
        search: (prev: LibrarySearch) => compact({ ...prev, page: n > 1 ? n : undefined }),
      }),
    [navigate],
  );
  return { q, page, search, goTo };
}

/** How many books the filtered libraries hold (undefined until libraries load). */
export function useLibraryBookCount(library: number | undefined): number | undefined {
  const libraries = useBookLibraries();
  if (!libraries) return undefined;
  return libraries
    .filter((l) => library === undefined || l.id === library)
    .reduce((n, l) => n + l.book_count, 0);
}
