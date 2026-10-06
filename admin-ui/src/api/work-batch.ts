import { refKey } from '@/lib/book-route';
import { createBatcher } from './batcher';
import { api, WORKS_LIMIT } from './client';
import type { BookRef, BookWork } from './types';

// Which community work each owned book is, for the Series cards, is requested
// one per book (useBookWorks) but fetched in batches like the covers
// (batcher.ts): every book asked about in the same moment (a page of cards
// placing their books on their rails) goes out as one POST /admin/books/works of
// up to WORKS_LIMIT, instead of one request per card.

/** One book's community work, as the batch answered it. */
export interface WorkAnswer {
  /** The work id; "" when none is known. */
  id: string;
  /**
   * The answer stands: a work, or a clean "no work". False when the book's
   * lookup failed (or the request did), so asking again later may resolve it.
   */
  final: boolean;
}

const UNRESOLVED: WorkAnswer = { id: '', final: false };

/**
 * A book's community work, batched with every other book asked about in the
 * same moment. An aborted `signal` (the card went away before the batch went
 * out) drops this request, and a book nobody waits for any more isn't sent. A
 * failed request is not an error: its books are unresolved and not final.
 */
export const loadWork = createBatcher<BookRef, WorkAnswer>({
  key: refKey,
  limit: WORKS_LIMIT,
  send: async (refs) => {
    let byKey: Map<string, BookWork>;
    try {
      const { works } = await api.bookWorks(refs);
      byKey = new Map((works ?? []).map((w) => [refKey(w), w]));
    } catch {
      return refs.map(() => UNRESOLVED);
    }
    return refs.map((r) => {
      const w = byKey.get(refKey(r));
      // Only the book's own failure is asked about again, never a clean miss.
      return w ? { id: w.work_id, final: !w.failed } : UNRESOLVED;
    });
  },
});
