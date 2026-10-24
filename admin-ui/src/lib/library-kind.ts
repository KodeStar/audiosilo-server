import type { Library } from '@/api/types';

/**
 * Whether a library holds books. A podcast library's episodes aren't books, so
 * the screens and actions about books (Library > Books, Authors, Narrators,
 * Series, Folders, matching, Overview's books per library) leave it out. A
 * library with no kind (a server before 2.4.0) holds books.
 */
export const isBookLibrary = (l: { kind?: Library['kind'] }) => l.kind !== 'podcasts';
