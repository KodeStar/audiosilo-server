import type { AdminLibrary } from '@/api/types';

/**
 * Whether a library holds books. A podcast library's episodes aren't books, so
 * the screens and actions about books (Library > Books, Authors, Narrators,
 * Series, Folders, matching) leave it out.
 */
export const isBookLibrary = (l: Pick<AdminLibrary, 'kind'>) => l.kind !== 'podcasts';
