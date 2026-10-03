import { api } from './client';
import type { BookRef } from './types';

// Cover thumbnails are requested one per <BookCover> but fetched in batches:
// every cover asked for in the same moment (a grid page mounting, a shelf
// scrolling into view) goes out as one POST /admin/covers of up to 60. A grid of
// hundreds of covers would otherwise be hundreds of requests, which the server's
// per-IP limiter (burst 40) refuses, and each would be full-size art for a
// 158px tile.

export type ThumbSize = 160 | 320 | 640;

/** The server's per-request cap (handlers_covers.go maxCoverBatch). */
export const MAX_COVER_BATCH = 60;
/** How long to collect requests before sending: one frame or so. */
const COLLECT_MS = 16;

interface Waiter {
  resolve: (dataUrl: string | null) => void;
  reject: (err: unknown) => void;
}

const keyOf = (ref: BookRef) => `${ref.library_id}\u0000${ref.path}`;

/** Pending requests per size, by book; several waiters can share one book. */
const pending = new Map<ThumbSize, Map<string, { ref: BookRef; waiters: Waiter[] }>>();
let timer: ReturnType<typeof setTimeout> | undefined;

/** A book's cover thumbnail as a data: URL, or null when it has no art. */
export function loadThumb(ref: BookRef, size: ThumbSize): Promise<string | null> {
  return new Promise((resolve, reject) => {
    let bySize = pending.get(size);
    if (!bySize) pending.set(size, (bySize = new Map()));
    const key = keyOf(ref);
    const entry = bySize.get(key) ?? { ref, waiters: [] };
    entry.waiters.push({ resolve, reject });
    bySize.set(key, entry);
    timer ??= setTimeout(flush, COLLECT_MS);
  });
}

function flush() {
  timer = undefined;
  const batches = [...pending];
  pending.clear();
  for (const [size, bySize] of batches) {
    const entries = [...bySize.values()];
    for (let i = 0; i < entries.length; i += MAX_COVER_BATCH) {
      void send(entries.slice(i, i + MAX_COVER_BATCH), size);
    }
  }
}

async function send(entries: { ref: BookRef; waiters: Waiter[] }[], size: ThumbSize) {
  try {
    const { covers } = await api.coverThumbs(
      entries.map((e) => e.ref),
      size,
    );
    const byKey = new Map((covers ?? []).map((c) => [keyOf(c), c.data || null]));
    for (const e of entries) {
      const data = byKey.get(keyOf(e.ref)) ?? null;
      for (const w of e.waiters) w.resolve(data);
    }
  } catch (err) {
    for (const e of entries) for (const w of e.waiters) w.reject(err);
  }
}
