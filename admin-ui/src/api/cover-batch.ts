import { refKey } from '@/lib/book-route';
import { createBatcher, type Batched } from './batcher';
import { api, type ThumbSize } from './client';
import type { BookRef } from './types';

// Cover thumbnails are requested one per <BookCover> but fetched in batches
// (batcher.ts): every cover asked for in the same moment (a grid page mounting,
// a shelf scrolling into view) goes out as one POST /admin/covers of up to 60,
// one batch per size. A grid of hundreds of covers would otherwise be hundreds
// of requests, which the server's per-IP limiter (burst 40) refuses, and each
// would be full-size art for a 158px tile.

/** The server's per-request cap (handlers_covers.go maxCoverBatch). */
export const MAX_COVER_BATCH = 60;

/** One batcher per size: a request carries a single size. */
const bySize = new Map<ThumbSize, Batched<BookRef, string | null>>();

/**
 * A book's cover thumbnail as a data: URL, or null when it has no art. An
 * aborted `signal` (the cover scrolled away before the batch went out) drops
 * this request, and a book nobody waits for any more isn't sent.
 */
export function loadThumb(
  ref: BookRef,
  size: ThumbSize,
  signal?: AbortSignal,
): Promise<string | null> {
  let load = bySize.get(size);
  if (!load) {
    load = createBatcher({
      key: refKey,
      limit: MAX_COVER_BATCH,
      send: (refs: BookRef[]) => sendCovers(refs, size),
    });
    bySize.set(size, load);
  }
  return load(ref, signal);
}

async function sendCovers(refs: BookRef[], size: ThumbSize) {
  const { covers } = await api.coverThumbs(refs, size);
  const byKey = new Map((covers ?? []).map((c) => [refKey(c), c.data || null]));
  return refs.map((r) => byKey.get(refKey(r)) ?? null);
}
