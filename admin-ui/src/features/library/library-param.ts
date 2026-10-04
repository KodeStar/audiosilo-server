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

/** The Library screens' `?library=` filter: one library's id, or undefined for all. */
export function useLibraryParam(): [number | undefined, (id: number | undefined) => void] {
  const { library } = useSearch({ strict: false }) as LibrarySearch;
  const update = useUpdateSearch();
  const set = useCallback(
    (id: number | undefined) => update((prev) => ({ ...prev, library: id })),
    [update],
  );
  return [library, set];
}

/** How many books the filtered libraries hold (undefined until libraries load). */
export function useLibraryBookCount(library: number | undefined): number | undefined {
  const libraries = useLibraries();
  if (!libraries.data) return undefined;
  return libraries.data
    .filter((l) => library === undefined || l.id === library)
    .reduce((n, l) => n + l.book_count, 0);
}
