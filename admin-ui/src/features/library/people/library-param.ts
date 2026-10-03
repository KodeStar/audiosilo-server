import { useNavigate, useSearch } from '@tanstack/react-router';
import { useLibraries } from '@/api/hooks';

/** The Library screens' `?library=` filter: one library's id, or undefined for all. */
export function useLibraryParam(): [number | undefined, (id: number | undefined) => void] {
  const { library } = useSearch({ strict: false }) as { library?: number };
  const navigate = useNavigate();
  const set = (id: number | undefined) =>
    void navigate({
      to: '.',
      search: (prev: Record<string, unknown>) => ({ ...prev, library: id }),
      replace: true,
    });
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
