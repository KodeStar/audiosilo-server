import { IDBFactory } from 'fake-indexeddb';
import {
  MAX_AGE_MS,
  ThumbCache,
  memoryStore,
  openIndexedDBStore,
  thumbKey,
  type ThumbStore,
} from './thumb-cache';

// The console's thumbnail cache: keyed by book, size and cover_version, bounded
// by count and bytes (least recently used out first), kept across reloads.

const dune = { library_id: 1, path: 'Herbert/Dune' };
const big = { maxEntries: 1000, maxBytes: 1 << 30 };

function clock(start = 1_000_000) {
  let t = start;
  return {
    now: () => t,
    advance: (ms: number) => {
      t += ms;
    },
  };
}

describe('ThumbCache', () => {
  it('answers a stored thumbnail by book, size and version, and nothing else', async () => {
    const cache = new ThumbCache(memoryStore(), big);
    await cache.put(thumbKey(dune, 320, 'v1'), 'data:a');
    expect(await cache.get(thumbKey(dune, 320, 'v1'))).toBe('data:a');
    // New art is a new version: a miss, not the old picture.
    expect(await cache.get(thumbKey(dune, 320, 'v2'))).toBeUndefined();
    expect(await cache.get(thumbKey(dune, 160, 'v1'))).toBeUndefined();
    expect(await cache.get(thumbKey({ ...dune, library_id: 2 }, 320, 'v1'))).toBeUndefined();
  });

  it('keeps its thumbnails across a reload of the page', async () => {
    const store = memoryStore();
    await new ThumbCache(store, big).put(thumbKey(dune, 96, 'v1'), 'data:a');
    const reloaded = new ThumbCache(store, big);
    expect(await reloaded.get(thumbKey(dune, 96, 'v1'))).toBe('data:a');
    expect(await reloaded.size()).toEqual({ entries: 1, bytes: 'data:a'.length });
  });

  it('evicts the least recently used down to 90% once over the count', async () => {
    const t = clock();
    const store = memoryStore();
    const cache = new ThumbCache(store, { maxEntries: 10, maxBytes: 1 << 30 }, t.now);
    const key = (i: number) => thumbKey({ library_id: 1, path: `b${i}` }, 320, 'v');
    for (let i = 0; i < 10; i++) {
      await cache.put(key(i), `data:${i}`);
      t.advance(2 * 60 * 60_000); // past the touch interval, so a read refreshes the age
    }
    // Reading the oldest makes it recent.
    expect(await cache.get(key(0))).toBe('data:0');
    await cache.put(key(10), 'data:10');
    expect((await cache.size()).entries).toBe(9);
    expect(await cache.get(key(0))).toBe('data:0');
    expect(await cache.get(key(1))).toBeUndefined();
    expect(await cache.get(key(2))).toBeUndefined();
    expect(await cache.get(key(10))).toBe('data:10');
    // Evicted from the store too, not just forgotten.
    expect(await store.get(key(1))).toBeUndefined();
  });

  it('evicts by bytes as well as count', async () => {
    const t = clock();
    const cache = new ThumbCache(memoryStore(), { maxEntries: 100, maxBytes: 1000 }, t.now);
    for (let i = 0; i < 4; i++) {
      await cache.put(thumbKey({ library_id: 1, path: `b${i}` }, 320, 'v'), 'x'.repeat(300));
      t.advance(1);
    }
    expect(await cache.size()).toEqual({ entries: 3, bytes: 900 });
    expect(await cache.get(thumbKey({ library_id: 1, path: 'b0' }, 320, 'v'))).toBeUndefined();
  });

  it('replaces an entry without counting it twice', async () => {
    const cache = new ThumbCache(memoryStore(), big);
    await cache.put(thumbKey(dune, 320, 'v1'), 'data:aa');
    await cache.put(thumbKey(dune, 320, 'v1'), 'data:b');
    expect(await cache.size()).toEqual({ entries: 1, bytes: 'data:b'.length });
  });

  it('asks again for a thumbnail stored longer ago than the maximum age', async () => {
    const t = clock();
    const cache = new ThumbCache(memoryStore(), big, t.now);
    await cache.put(thumbKey(dune, 320, 'v1'), 'data:a');
    t.advance(MAX_AGE_MS - 1);
    expect(await cache.get(thumbKey(dune, 320, 'v1'))).toBe('data:a');
    t.advance(2);
    expect(await cache.get(thumbKey(dune, 320, 'v1'))).toBeUndefined();
    expect((await cache.size()).entries).toBe(0);
  });

  it("forgets one book's thumbnails, every size and version, and clears all", async () => {
    const store = memoryStore();
    const cache = new ThumbCache(store, big);
    const other = { library_id: 1, path: 'Herbert/Dune Messiah' };
    await cache.put(thumbKey(dune, 320, 'v1'), 'data:a');
    await cache.put(thumbKey(dune, 96, 'v2'), 'data:b');
    await cache.put(thumbKey(other, 320, 'v1'), 'data:c');
    await cache.forget(dune.library_id, dune.path);
    expect(await cache.get(thumbKey(dune, 320, 'v1'))).toBeUndefined();
    expect(await cache.get(thumbKey(dune, 96, 'v2'))).toBeUndefined();
    // A path that merely starts with the forgotten one is another book.
    expect(await cache.get(thumbKey(other, 320, 'v1'))).toBe('data:c');
    await cache.clear();
    expect(await cache.size()).toEqual({ entries: 0, bytes: 0 });
    expect(await store.entries()).toEqual([]);
    expect(await new ThumbCache(store, big).get(thumbKey(other, 320, 'v1'))).toBeUndefined();
  });

  it('misses rather than fails when the store does', async () => {
    const broken: ThumbStore = {
      entries: () => Promise.reject(new Error('blocked')),
      get: () => Promise.reject(new Error('blocked')),
      put: () => Promise.reject(new Error('blocked')),
      touch: () => Promise.reject(new Error('blocked')),
      remove: () => Promise.reject(new Error('blocked')),
      clear: () => Promise.reject(new Error('blocked')),
    };
    const cache = new ThumbCache(broken, big);
    await cache.put(thumbKey(dune, 320, 'v1'), 'data:a');
    expect(await cache.get(thumbKey(dune, 320, 'v1'))).toBeUndefined();
    await expect(cache.clear()).resolves.toBeUndefined();
  });

  it('drops an entry another tab removed from the store', async () => {
    const store = memoryStore();
    const cache = new ThumbCache(store, big);
    await cache.put(thumbKey(dune, 320, 'v1'), 'data:a');
    await store.remove([thumbKey(dune, 320, 'v1')]);
    expect(await cache.get(thumbKey(dune, 320, 'v1'))).toBeUndefined();
    expect((await cache.size()).entries).toBe(0);
  });
});

describe('openIndexedDBStore', () => {
  it('keeps entries in IndexedDB across opens, and removes and clears them', async () => {
    const idb = new IDBFactory();
    const store = await openIndexedDBStore(idb);
    const cache = new ThumbCache(store, big);
    await cache.put(thumbKey(dune, 320, 'v1'), 'data:a');
    await cache.put(thumbKey(dune, 96, 'v1'), 'data:b');

    // A reload: a new connection, the index read back without the data.
    const again = await openIndexedDBStore(idb);
    const entries = await again.entries();
    expect(entries.map((e) => e.key).sort()).toEqual(
      [thumbKey(dune, 320, 'v1'), thumbKey(dune, 96, 'v1')].sort(),
    );
    expect(entries.every((e) => !('data' in e))).toBe(true);
    const reloaded = new ThumbCache(again, big);
    expect(await reloaded.get(thumbKey(dune, 320, 'v1'))).toBe('data:a');

    await again.remove([thumbKey(dune, 96, 'v1')]);
    expect(await again.get(thumbKey(dune, 96, 'v1'))).toBeUndefined();
    expect(await again.entries()).toHaveLength(1);
    await reloaded.clear();
    expect(await (await openIndexedDBStore(idb)).entries()).toEqual([]);
  });
});
