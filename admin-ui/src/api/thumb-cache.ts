import type { BookRef } from './types';
import type { ThumbSize } from './client';

// The console's cover thumbnails, kept across reloads. POST /admin/covers
// answers with data: URLs inside JSON (the console's bearer token must never
// ride in an <img> URL, and its CSP takes images only from 'self' and data:),
// so the browser's HTTP cache never holds them: without this every reload or
// re-scroll fetched every cover again. Entries are keyed by the book, the size
// and the row's cover_version (catalog.CoverVersion, which changes with the
// art), so new art is simply a new key and the old one ages out. Kept in
// IndexedDB (it works over plain HTTP on a LAN address, where Cache Storage
// doesn't exist), else in memory; bounded by count and bytes, least recently
// used out first; cleared on sign-out.

/** What the cache knows of an entry without reading its data. */
export interface ThumbEntry {
  key: string;
  /** The data: URL's length (base64, so its size in bytes). */
  bytes: number;
  /** When it was last stored or read (ms), for least-recently-used eviction. */
  at: number;
  /** When it was stored (ms): past MAX_AGE_MS it is asked for again. */
  stored: number;
}

/** Where a ThumbCache keeps its entries. Any method may reject: the cache then misses. */
export interface ThumbStore {
  /** Every entry, without its data. */
  entries(): Promise<ThumbEntry[]>;
  get(key: string): Promise<string | undefined>;
  put(entry: ThumbEntry, data: string): Promise<void>;
  touch(entry: ThumbEntry): Promise<void>;
  remove(keys: string[]): Promise<void>;
  clear(): Promise<void>;
}

export interface ThumbLimits {
  maxEntries: number;
  maxBytes: number;
}

/** IndexedDB's bound: ~5k covers or ~50 MB, whichever comes first. */
export const STORED_LIMITS: ThumbLimits = { maxEntries: 5000, maxBytes: 50 << 20 };
/** In memory (no IndexedDB) the bound is smaller: it is the page's heap. */
export const MEMORY_LIMITS: ThumbLimits = { maxEntries: 1500, maxBytes: 16 << 20 };

/** A read refreshes an entry's age at most this often (a read is otherwise not a write). */
const TOUCH_MS = 60 * 60_000;

/**
 * How long a stored thumbnail is used before it is asked for again. The
 * version changes with the art the index knows of, but a sidecar image
 * replaced in place under the same name moves it only once the server makes a
 * thumbnail of the new art, which a cache that never asks would never see.
 */
export const MAX_AGE_MS = 7 * 24 * 60 * 60_000;

/** The cache key of a book's thumbnail at a size for its cover_version. */
export function thumbKey(ref: BookRef, size: ThumbSize, version: string) {
  return `${bookPrefix(ref.library_id, ref.path)}${size}\u0000${version}`;
}

/** The start of every key of one book's thumbnails (paths never hold a NUL). */
function bookPrefix(libraryId: number, path: string) {
  return `${libraryId}\u0000${path}\u0000`;
}

/**
 * A bounded thumbnail cache over a ThumbStore. The entries' keys, sizes and
 * ages are read once and kept in memory, so a miss is known without a read and
 * eviction needs no scan of the store.
 */
export class ThumbCache {
  private index: Promise<Map<string, ThumbEntry>> | undefined;
  private bytes = 0;
  private store: ThumbStore;
  private limits: ThumbLimits;
  private now: () => number;

  /** `limits` is read on every put, so the owner may lower it later (the memory fallback). */
  constructor(store: ThumbStore, limits: ThumbLimits, now: () => number = Date.now) {
    this.store = store;
    this.limits = limits;
    this.now = now;
  }

  private load() {
    this.index ??= this.store.entries().then(
      (list) => {
        const m = new Map(list.map((e) => [e.key, e]));
        this.bytes = list.reduce((n, e) => n + e.bytes, 0);
        return m;
      },
      () => new Map<string, ThumbEntry>(),
    );
    return this.index;
  }

  /** The thumbnail stored under key, or undefined. */
  async get(key: string): Promise<string | undefined> {
    const index = await this.load();
    const entry = index.get(key);
    if (!entry) return undefined;
    const now = this.now();
    if (now - entry.stored > MAX_AGE_MS) {
      await this.remove(index, [key]);
      return undefined;
    }
    const data = await this.store.get(key).catch(() => undefined);
    if (data === undefined) {
      // Gone from the store (another tab evicted it, or a failed read).
      this.drop(index, [key]);
      return undefined;
    }
    if (now - entry.at > TOUCH_MS) {
      entry.at = now;
      void this.store.touch({ ...entry }).catch(() => {});
    }
    return data;
  }

  /** Keeps a thumbnail under key, then evicts down to the limits. */
  async put(key: string, data: string): Promise<void> {
    const index = await this.load();
    const old = index.get(key);
    if (old) this.bytes -= old.bytes;
    const now = this.now();
    const entry: ThumbEntry = { key, bytes: data.length, at: now, stored: now };
    index.set(key, entry);
    this.bytes += entry.bytes;
    await this.store.put(entry, data).catch(() => {
      // Not kept (quota, a closed database): forget it, the next view asks again.
      this.drop(index, [key]);
    });
    await this.evict(index);
  }

  /** Forgets every thumbnail of one book (a new cover was set or removed). */
  async forget(libraryId: number, path: string): Promise<void> {
    const index = await this.load();
    const prefix = bookPrefix(libraryId, path);
    const keys = [...index.keys()].filter((k) => k.startsWith(prefix));
    if (keys.length) await this.remove(index, keys);
  }

  /** Forgets everything (sign-out). */
  async clear(): Promise<void> {
    const index = await this.load();
    index.clear();
    this.bytes = 0;
    await this.store.clear().catch(() => {});
  }

  /** How many entries and bytes it holds (tests). */
  async size() {
    const index = await this.load();
    return { entries: index.size, bytes: this.bytes };
  }

  /**
   * Over a limit, removes the least recently used entries down to 90% of both,
   * so a full cache evicts in batches rather than on every put.
   */
  private async evict(index: Map<string, ThumbEntry>) {
    const { maxEntries, maxBytes } = this.limits;
    if (index.size <= maxEntries && this.bytes <= maxBytes) return;
    const entries = Math.floor(maxEntries * 0.9);
    const bytes = Math.floor(maxBytes * 0.9);
    const oldest = [...index.values()].sort((a, b) => a.at - b.at);
    const out: string[] = [];
    let n = index.size;
    let b = this.bytes;
    for (const e of oldest) {
      if (n <= entries && b <= bytes) break;
      out.push(e.key);
      n--;
      b -= e.bytes;
    }
    await this.remove(index, out);
  }

  private async remove(index: Map<string, ThumbEntry>, keys: string[]) {
    this.drop(index, keys);
    await this.store.remove(keys).catch(() => {});
  }

  private drop(index: Map<string, ThumbEntry>, keys: string[]) {
    for (const k of keys) {
      const e = index.get(k);
      if (!e) continue;
      this.bytes -= e.bytes;
      index.delete(k);
    }
  }
}

/** A ThumbStore in memory: the fallback without IndexedDB, and the tests'. */
export function memoryStore(): ThumbStore {
  const meta = new Map<string, ThumbEntry>();
  const data = new Map<string, string>();
  return {
    entries: async () => [...meta.values()].map((e) => ({ ...e })),
    get: async (key) => data.get(key),
    put: async (entry, d) => {
      meta.set(entry.key, { ...entry });
      data.set(entry.key, d);
    },
    touch: async (entry) => {
      if (meta.has(entry.key)) meta.set(entry.key, { ...entry });
    },
    remove: async (keys) => {
      for (const k of keys) {
        meta.delete(k);
        data.delete(k);
      }
    },
    clear: async () => {
      meta.clear();
      data.clear();
    },
  };
}

const DB_NAME = 'audiosilo-admin-thumbs';
/** Entries' keys, sizes and ages (read whole on open) apart from their data (read per cover). */
const META = 'meta';
const DATA = 'data';

function done(req: IDBRequest): Promise<void>;
function done<T>(req: IDBRequest<T>): Promise<T>;
function done<T>(req: IDBRequest<T>) {
  return new Promise<T>((resolve, reject) => {
    req.onsuccess = () => resolve(req.result);
    req.onerror = () => reject(req.error);
  });
}

function committed(tx: IDBTransaction) {
  return new Promise<void>((resolve, reject) => {
    tx.oncomplete = () => resolve();
    tx.onerror = () => reject(tx.error);
    tx.onabort = () => reject(tx.error ?? new Error('aborted'));
  });
}

/** Opens the console's thumbnail database; rejects where IndexedDB can't be used. */
export function openIndexedDBStore(idb: IDBFactory = indexedDB): Promise<ThumbStore> {
  const open = idb.open(DB_NAME, 1);
  open.onupgradeneeded = () => {
    open.result.createObjectStore(META, { keyPath: 'key' });
    open.result.createObjectStore(DATA);
  };
  return done(open).then((db) => {
    // Another tab's newer version (none yet) or a cleared site: let it go.
    db.onversionchange = () => db.close();
    const write = (fn: (meta: IDBObjectStore, data: IDBObjectStore) => void) => {
      const tx = db.transaction([META, DATA], 'readwrite');
      fn(tx.objectStore(META), tx.objectStore(DATA));
      return committed(tx);
    };
    return {
      entries: () =>
        done(db.transaction(META).objectStore(META).getAll() as IDBRequest<ThumbEntry[]>),
      get: (key) =>
        done(db.transaction(DATA).objectStore(DATA).get(key) as IDBRequest<string | undefined>),
      put: (entry, data) =>
        write((meta, d) => {
          meta.put(entry);
          d.put(data, entry.key);
        }),
      touch: (entry) => {
        const tx = db.transaction(META, 'readwrite');
        tx.objectStore(META).put(entry);
        return committed(tx);
      },
      remove: (keys) =>
        write((meta, d) => {
          for (const k of keys) {
            meta.delete(k);
            d.delete(k);
          }
        }),
      clear: () =>
        write((meta, d) => {
          meta.clear();
          d.clear();
        }),
    } satisfies ThumbStore;
  });
}

/**
 * A ThumbStore that opens IndexedDB on first use and, where that fails
 * (private browsing in some browsers, blocked site data), keeps the entries in
 * memory instead, with the smaller limits.
 */
function fallbackStore(onMemory: () => void): ThumbStore {
  let store: Promise<ThumbStore> | undefined;
  const get = () =>
    (store ??= (
      typeof indexedDB === 'undefined'
        ? Promise.reject(new Error('no IndexedDB'))
        : openIndexedDBStore()
    ).catch(() => {
      onMemory();
      return memoryStore();
    }));
  return {
    entries: () => get().then((s) => s.entries()),
    get: (key) => get().then((s) => s.get(key)),
    put: (entry, data) => get().then((s) => s.put(entry, data)),
    touch: (entry) => get().then((s) => s.touch(entry)),
    remove: (keys) => get().then((s) => s.remove(keys)),
    clear: () => get().then((s) => s.clear()),
  };
}

let shared: ThumbCache | undefined;

/** The console's one thumbnail cache. */
export function thumbCache(): ThumbCache {
  if (!shared) {
    const limits = { ...STORED_LIMITS };
    shared = new ThumbCache(
      fallbackStore(() => Object.assign(limits, MEMORY_LIMITS)),
      limits,
    );
  }
  return shared;
}

/** Empties the thumbnail cache (sign-out: the next admin starts with none). */
export function clearThumbCache(): Promise<void> {
  return thumbCache().clear();
}
