import { mockFetch } from '@/test/fetch-mock';
import type { BookRef } from './types';
import { MAX_COVER_BATCH, loadThumb } from './cover-batch';
import { clearThumbCache, thumbCache } from './thumb-cache';

// Covers requested together go out as one POST /admin/covers per 60 books (and
// per size), each request answered from the batch it rode in.

/** The server's answer, each cover of the book's current cover_version (`version`). */
function coversRoute(version = 'v1') {
  return (req: { body: unknown }) => ({
    body: {
      covers: (req.body as { books: BookRef[] }).books.map((b) =>
        b.path === 'none'
          ? { ...b, data: '' }
          : { ...b, data: `data:image/jpeg;base64,${b.path}`, version },
      ),
    },
  });
}

/** Lets the puts loadThumb doesn't wait for land. */
const settle = () => new Promise((r) => setTimeout(r, 0));

afterEach(async () => {
  vi.unstubAllGlobals();
  await clearThumbCache();
});

describe('loadThumb', () => {
  it('batches covers asked for together, and resolves each from the answer', async () => {
    const calls = mockFetch({ 'POST /admin/covers': coversRoute() });
    const [a, b, none, again] = await Promise.all([
      loadThumb({ library_id: 1, path: 'a' }, 320),
      loadThumb({ library_id: 2, path: 'b' }, 320),
      loadThumb({ library_id: 1, path: 'none' }, 320),
      loadThumb({ library_id: 1, path: 'a' }, 320), // the same book twice: asked once
    ]);
    expect([a, b, none, again]).toEqual([
      'data:image/jpeg;base64,a',
      'data:image/jpeg;base64,b',
      null,
      'data:image/jpeg;base64,a',
    ]);
    expect(calls).toHaveLength(1);
    expect(calls[0].body).toEqual({
      books: [
        { library_id: 1, path: 'a' },
        { library_id: 2, path: 'b' },
        { library_id: 1, path: 'none' },
      ],
      size: 320,
    });
  });

  it('splits more than a batch, and keeps sizes apart', async () => {
    const calls = mockFetch({ 'POST /admin/covers': coversRoute() });
    await Promise.all([
      ...Array.from({ length: MAX_COVER_BATCH + 1 }, (_, i) =>
        loadThumb({ library_id: 1, path: `p${i}` }, 320),
      ),
      loadThumb({ library_id: 1, path: 'p0' }, 160),
    ]);
    expect(
      calls.map((c) => [
        (c.body as { books: unknown[] }).books.length,
        (c.body as { size: number }).size,
      ]),
    ).toEqual([
      [MAX_COVER_BATCH, 320],
      [1, 320],
      [1, 160],
    ]);
  });

  it('fails every cover of a batch the server refused', async () => {
    mockFetch({ 'POST /admin/covers': { status: 500, body: { error: 'could not load covers' } } });
    const results = await Promise.allSettled([
      loadThumb({ library_id: 1, path: 'a' }, 320),
      loadThumb({ library_id: 1, path: 'b' }, 320),
    ]);
    expect(results.map((r) => r.status)).toEqual(['rejected', 'rejected']);
  });

  it('drops an aborted request, and sends no book nobody waits for', async () => {
    const calls = mockFetch({ 'POST /admin/covers': coversRoute() });
    const gone = new AbortController();
    const both = new AbortController();
    const dropped = loadThumb({ library_id: 1, path: 'gone' }, 320, gone.signal);
    const shared = loadThumb({ library_id: 1, path: 'a' }, 320, both.signal);
    const kept = loadThumb({ library_id: 1, path: 'a' }, 320);
    gone.abort();
    both.abort();
    await expect(dropped).rejects.toBeDefined();
    await expect(shared).rejects.toBeDefined();
    // Another waiter still wants "a": it is sent, "gone" isn't.
    expect(await kept).toBe('data:image/jpeg;base64,a');
    expect(calls).toHaveLength(1);
    expect(calls[0].body).toEqual({ books: [{ library_id: 1, path: 'a' }], size: 320 });
  });

  it('keeps a versioned thumbnail, so asking again (a reload) sends nothing', async () => {
    const calls = mockFetch({ 'POST /admin/covers': coversRoute() });
    const a = { library_id: 1, path: 'a' };
    expect(await loadThumb(a, 96, undefined, 'v1')).toBe('data:image/jpeg;base64,a');
    await settle();
    expect(await loadThumb(a, 96, undefined, 'v1')).toBe('data:image/jpeg;base64,a');
    expect(calls).toHaveLength(1);
    // A new cover_version, another size, or no version at all asks the server.
    await loadThumb(a, 96, undefined, 'v2');
    await loadThumb(a, 320, undefined, 'v1');
    await loadThumb(a, 96);
    expect(calls).toHaveLength(4);
  });

  it('keeps no answer for a book without art (it may be unreadable for now)', async () => {
    const calls = mockFetch({ 'POST /admin/covers': coversRoute() });
    const none = { library_id: 1, path: 'none' };
    expect(await loadThumb(none, 160, undefined, 'v1')).toBeNull();
    await settle();
    expect(await loadThumb(none, 160, undefined, 'v1')).toBeNull();
    expect(calls).toHaveLength(2);
  });

  it("keeps art under the server's version, not an older row's", async () => {
    // The cover was just replaced: the server's art is of v2 while the row still says v1.
    const calls = mockFetch({ 'POST /admin/covers': coversRoute('v2') });
    const a = { library_id: 1, path: 'a' };
    expect(await loadThumb(a, 160, undefined, 'v1')).toBe('data:image/jpeg;base64,a');
    await settle();
    // Not kept under v1 (it would show if the cover went back to v1's)...
    await loadThumb(a, 160, undefined, 'v1');
    expect(calls).toHaveLength(2);
    // ...but under v2, which the refetched row names.
    await settle();
    expect(await loadThumb(a, 160, undefined, 'v2')).toBe('data:image/jpeg;base64,a');
    expect(calls).toHaveLength(2);
  });

  it('keeps nothing that arrives after a sign-out cleared the cache', async () => {
    let release: () => void = () => {};
    const gate = new Promise<void>((r) => (release = r));
    const route = coversRoute();
    const calls = mockFetch({
      'POST /admin/covers': async (req: { body: unknown }) => {
        await gate;
        return route(req);
      },
    });
    const a = { library_id: 1, path: 'a' };
    const late = loadThumb(a, 320, undefined, 'v1');
    await new Promise((r) => setTimeout(r, 50)); // the batch is out
    await clearThumbCache();
    release();
    expect(await late).toBe('data:image/jpeg;base64,a');
    await settle();
    expect((await thumbCache().size()).entries).toBe(0);
    expect(calls).toHaveLength(1);
  });

  it('still batches the misses of a page asked for together', async () => {
    const calls = mockFetch({ 'POST /admin/covers': coversRoute() });
    await loadThumb({ library_id: 1, path: 'p0' }, 320, undefined, 'v1');
    await settle();
    await Promise.all(
      Array.from({ length: 5 }, (_, i) =>
        loadThumb({ library_id: 1, path: `p${i}` }, 320, undefined, 'v1'),
      ),
    );
    expect(calls).toHaveLength(2);
    expect((calls[1].body as { books: BookRef[] }).books.map((b) => b.path)).toEqual([
      'p1',
      'p2',
      'p3',
      'p4',
    ]);
  });
});
