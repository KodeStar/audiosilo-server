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

/** One batcher per size: a request carries a single size. */
const bySize = new Map<ThumbSize, Batched<BookRef, string | null>>();

/**
 * A book's cover thumbnail as a data: URL, or null when it has no art. An
 * aborted `signal` (the cover scrolled away before the batch went out) drops
 * this request, and a book nobody waits for any more isn't sent. With the
 * row's cover `version` the thumbnail comes from, and goes into, the
 * thumbnail cache (thumb-cache.ts), so a reload doesn't fetch it again; a
 * book without art isn't kept (its art may only be unreadable for now).
 */
export async function loadThumb(
  ref: BookRef,
  size: ThumbSize,
  signal?: AbortSignal,
  version?: string,
): Promise<string | null> {
  const key = version ? thumbKey(ref, size, version) : undefined;
  if (key) {
    const hit = await thumbCache().get(key);
    if (hit) return hit;
  }
  const data = await batched(size)(ref, signal);
  if (key && data) void thumbCache().put(key, data);
  return data;
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

async function sendCovers(refs: BookRef[], size: ThumbSize) {
  const { covers } = await api.coverThumbs(refs, size);
  const byKey = new Map((covers ?? []).map((c) => [refKey(c), c.data || null]));
  return refs.map((r) => byKey.get(refKey(r)) ?? null);
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
