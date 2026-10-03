import type { BookRef } from '@/api/types';

/**
 * Link props for a book's page (`<Link {...bookRoute(…)}>`, `navigate(bookRoute(…))`).
 * The book is addressed by its identity, library + path, never an internal id.
 */
export function bookRoute(libraryId: number, path: string) {
  return { to: '/library/book', search: { library: libraryId, path } } as const;
}

export type BookSearch = { library: number; path: string };

/** The book page's search params, or undefined when they don't name a book. */
export function parseBookSearch(s: Record<string, unknown>): BookSearch | undefined {
  const library = Number(s.library);
  const path =
    typeof s.path === 'string' ? s.path : typeof s.path === 'number' ? String(s.path) : '';
  return Number.isInteger(library) && library > 0 && path ? { library, path } : undefined;
}

export const refOf = (b: { library_id: number; path: string }): BookRef => ({
  library_id: b.library_id,
  path: b.path,
});
