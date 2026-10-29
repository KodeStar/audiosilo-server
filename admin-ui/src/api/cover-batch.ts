import { refKey } from '@/lib/book-route';
import { createBatcher, type Batched } from './batcher';
import { COMMUNITY_COVERS_LIMIT, api, type ThumbSize } from './client';
import { thumbCache, thumbKey } from './thumb-cache';
import type { BookRef } from './types';

// Cover thumbnails are requested one per <BookCover> but fetched in batches
// (batcher.ts): every cover asked for in the same moment (a grid page mounting,
// a shelf scrolling into view) goes out as one POST /admin/covers of up to 60,
// one batch per size. A grid of hundreds of covers would otherwise be hundreds
// of requests, which the server's per-IP limiter (burst 40) refuses, and each
// would be full-size art for a 158px tile.

/** The server's per-request cap (handlers_covers.go maxCoverBatch). */
export const MAX_COVER_BATCH = 60;

/** A thumbnail as the batch answers it: its data: URL and the cover_version it is of. */
interface Thumb {
  data: string | null;
  version?: string;
}

/** One batcher per size: a request carries a single size. */
const bySize = new Map<ThumbSize, Batched<BookRef, Thumb>>();

/**
 * A book's cover thumbnail as a data: URL, or null when it has no art. An
 * aborted `signal` (the cover scrolled away before the batch went out) drops
 * this request, and a book nobody waits for any more isn't sent. With the
 * row's cover `version` the thumbnail comes from, and goes into, the
 * thumbnail cache (thumb-cache.ts), so a reload doesn't fetch it again; a
 * book without art isn't kept (its art may only be unreadable for now). It is
 * kept under the version the server names, which is not the row's when the row
 * is older than the art (a cover just replaced, refetched before its row; a
 * first thumbnail, which moves the version to the art's own), so the next
 * row's version finds it. A sign-out while it was on its way keeps nothing.
 */
export async function loadThumb(
  ref: BookRef,
  size: ThumbSize,
  signal?: AbortSignal,
  version?: string,
): Promise<string | null> {
  const key = version ? thumbKey(ref, size, version) : undefined;
  const cache = thumbCache();
  const epoch = cache.epoch;
  if (key) {
    const hit = await cache.get(key);
    if (hit) return hit;
  }
  const thumb = await batched(size)(ref, signal);
  if (key && thumb.data && thumb.version) {
    void cache.put(thumbKey(ref, size, thumb.version), thumb.data, epoch);
  }
  return thumb.data;
}

function batched(size: ThumbSize) {
  let load = bySize.get(size);
  if (!load) {
    load = createBatcher({
      key: refKey,
      limit: MAX_COVER_BATCH,
      send: (refs: BookRef[]) => sendCovers(refs, size),
    });
    bySize.set(size, load);
  }
  return load;
}

async function sendCovers(refs: BookRef[], size: ThumbSize): Promise<Thumb[]> {
  const { covers } = await api.coverThumbs(refs, size);
  const byKey = new Map(
    (covers ?? []).map((c): [string, Thumb] => [
      refKey(c),
      { data: c.data || null, version: c.version },
    ]),
  );
  return refs.map((r) => byKey.get(refKey(r)) ?? { data: null });
}

/** One community-cover batcher per size (POST /admin/meta/covers carries one size). */
const communityBySize = new Map<ThumbSize, Batched<string, string | null>>();

/**
 * A community cover image (a match candidate's cover_url) as a thumbnail data:
 * URL the server fetched, or null when it couldn't; batched like loadThumb, at
 * most COMMUNITY_COVERS_LIMIT a request.
 */
export function loadCommunityCover(
  url: string,
  size: ThumbSize,
  signal?: AbortSignal,
): Promise<string | null> {
  let load = communityBySize.get(size);
  if (!load) {
    load = createBatcher({
      key: (u: string) => u,
      limit: COMMUNITY_COVERS_LIMIT,
      send: async (urls: string[]) => {
        const { covers } = await api.communityCovers(urls, size);
        return urls.map((_, i) => covers?.[i] || null);
      },
    });
    communityBySize.set(size, load);
  }
  return load(url, signal);
}
