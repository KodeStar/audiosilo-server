import { refKey } from '@/lib/book-route';
import { chunk } from '@/lib/utils';
import { api, type ThumbSize } from './client';
import type { BookRef } from './types';

// Cover thumbnails are requested one per <BookCover> but fetched in batches:
// every cover asked for in the same moment (a grid page mounting, a shelf
// scrolling into view) goes out as one POST /admin/covers of up to 60. A grid of
// hundreds of covers would otherwise be hundreds of requests, which the server's
// per-IP limiter (burst 40) refuses, and each would be full-size art for a
// 158px tile.

/** The server's per-request cap (handlers_covers.go maxCoverBatch). */
export const MAX_COVER_BATCH = 60;
/** How long to collect requests before sending: one frame or so. */
const COLLECT_MS = 16;

interface Waiter {
  resolve: (dataUrl: string | null) => void;
  reject: (err: unknown) => void;
}

interface Entry {
  ref: BookRef;
  waiters: Set<Waiter>;
}

/** Pending requests per size, by book; several waiters can share one book. */
const pending = new Map<ThumbSize, Map<string, Entry>>();
let timer: ReturnType<typeof setTimeout> | undefined;

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
  return new Promise((resolve, reject) => {
    if (signal?.aborted) return reject(signal.reason);
    let bySize = pending.get(size);
    if (!bySize) pending.set(size, (bySize = new Map()));
    const key = refKey(ref);
    const entry = bySize.get(key) ?? { ref, waiters: new Set<Waiter>() };
    bySize.set(key, entry);
    const waiter: Waiter = { resolve, reject };
    entry.waiters.add(waiter);
    signal?.addEventListener(
      'abort',
      () => {
        if (entry.waiters.delete(waiter)) reject(signal.reason);
      },
      { once: true },
    );
    timer ??= setTimeout(flush, COLLECT_MS);
  });
}

function flush() {
  timer = undefined;
  const batches = [...pending];
  pending.clear();
  for (const [size, bySize] of batches) {
    const wanted = [...bySize.values()].filter((e) => e.waiters.size > 0);
    for (const entries of chunk(wanted, MAX_COVER_BATCH)) void send(entries, size);
  }
}

async function send(entries: Entry[], size: ThumbSize) {
  try {
    const { covers } = await api.coverThumbs(
      entries.map((e) => e.ref),
      size,
    );
    const byKey = new Map((covers ?? []).map((c) => [refKey(c), c.data || null]));
    for (const e of entries) {
      const data = byKey.get(refKey(e.ref)) ?? null;
      for (const w of e.waiters) w.resolve(data);
    }
  } catch (err) {
    for (const e of entries) for (const w of e.waiters) w.reject(err);
  }
}
