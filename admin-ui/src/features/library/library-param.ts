import { useCallback } from 'react';
import { useNavigate, useSearch } from '@tanstack/react-router';
import { useLibraries } from '@/api/hooks';
import { compact } from '@/lib/utils';
import type { LibrarySearch, Update } from './library-search';

/**
 * Writes the Library screens' search params from the previous ones, dropping
 * what the change unsets, in place (a filter isn't a history entry).
 */
export function useUpdateSearch(): Update {
  const navigate = useNavigate();
  return useCallback(
    (fn) =>
      void navigate({
        to: '.',
        search: (prev: LibrarySearch) => compact(fn(prev)),
        replace: true,
      }),
    [navigate],
  );
}

/**
 * The Library screens' `?library=` filter: one library's id, or undefined for all.
 * Another library starts its list on the first page.
 */
export function useLibraryParam(): [number | undefined, (id: number | undefined) => void] {
  const { library } = useSearch({ strict: false }) as LibrarySearch;
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
  const libraries = useLibraries();
  if (!libraries.data) return undefined;
  return libraries.data
    .filter((l) => library === undefined || l.id === library)
    .reduce((n, l) => n + l.book_count, 0);
}
