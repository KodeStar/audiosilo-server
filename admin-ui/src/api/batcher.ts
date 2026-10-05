import { chunk } from '@/lib/utils';

// Per-item requests answered by batch endpoints (cover thumbnails, community
// works): each caller asks for one item, and every item asked for in the same
// moment goes out together, in chunks of at most `limit`. Several callers asking
// for one item share one slot in the batch.

/** How long to collect requests before sending: one frame or so. */
export const COLLECT_MS = 16;

export interface BatcherOptions<Item, Result> {
  /** An item's identity: callers asking for one key share one slot. */
  key: (item: Item) => string;
  /** The most items one `send` carries (the server's per-request cap). */
  limit: number;
  /**
   * Sends one chunk and answers each item, aligned to `items`. A throw rejects
   * every caller waiting on the chunk.
   */
  send: (items: Item[]) => Promise<Result[]>;
}

/** Asks for one item, batched; an aborted `signal` drops this caller's request. */
export type Batched<Item, Result> = (item: Item, signal?: AbortSignal) => Promise<Result>;

interface Waiter<Result> {
  resolve: (result: Result) => void;
  reject: (err: unknown) => void;
}

interface Entry<Item, Result> {
  item: Item;
  waiters: Set<Waiter<Result>>;
}

/**
 * A batched loader. An aborted `signal` (the caller went away before its answer
 * came) rejects that caller with the signal's reason, and an item nobody waits
 * for any more by the time the batch goes out isn't sent.
 */
export function createBatcher<Item, Result>({
  key,
  limit,
  send,
}: BatcherOptions<Item, Result>): Batched<Item, Result> {
  /** Pending items by key, in the order first asked for. */
  const pending = new Map<string, Entry<Item, Result>>();
  let timer: ReturnType<typeof setTimeout> | undefined;

  function flush() {
    timer = undefined;
    const wanted = [...pending.values()].filter((e) => e.waiters.size > 0);
    pending.clear();
    for (const entries of chunk(wanted, limit)) void run(entries);
  }

  async function run(entries: Entry<Item, Result>[]) {
    let results: Result[];
    try {
      results = await send(entries.map((e) => e.item));
    } catch (err) {
      for (const e of entries) for (const w of e.waiters) w.reject(err);
      return;
    }
    entries.forEach((e, i) => {
      for (const w of e.waiters) w.resolve(results[i]);
    });
  }

  return (item, signal) =>
    new Promise<Result>((resolve, reject) => {
      if (signal?.aborted) return reject(signal.reason);
      const k = key(item);
      const entry = pending.get(k) ?? { item, waiters: new Set<Waiter<Result>>() };
      pending.set(k, entry);
      // Once settled, the waiter stops listening: a long-lived signal (a page's)
      // must not collect a listener per answered request.
      const onAbort = () => {
        if (entry.waiters.delete(waiter)) reject(signal?.reason);
      };
      const waiter: Waiter<Result> = {
        resolve: (result) => {
          signal?.removeEventListener('abort', onAbort);
          resolve(result);
        },
        reject: (err) => {
          signal?.removeEventListener('abort', onAbort);
          reject(err);
        },
      };
      entry.waiters.add(waiter);
      signal?.addEventListener('abort', onAbort, { once: true });
      timer ??= setTimeout(flush, COLLECT_MS);
    });
}
