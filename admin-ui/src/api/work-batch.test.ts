import { QueryClient } from '@tanstack/react-query';
import { adminBook } from '@/test/library-fixtures';
import { mockFetch, type MockRequest } from '@/test/fetch-mock';
import { WORKS_LIMIT } from './client';
import { WORKS_RETRY_MS, bookWorkQuery } from './hooks';
import type { BookRef } from './types';
import { loadWork } from './work-batch';

// Books asked about together go out as one POST /admin/books/works per
// WORKS_LIMIT books, each answer read from the batch it rode in; the per-book
// queries keep a final answer and space out asking again about one that may
// have failed.

/** Each book is the work named like its path, except "none" (no work) and "down" (its lookup failed). */
function worksRoute() {
  return (req: MockRequest) => ({
    body: {
      works: (req.body as { books: BookRef[] }).books.map((b) => ({
        ...b,
        work_id: b.path === 'none' || b.path === 'down' ? '' : `w-${b.path}`,
        failed: b.path === 'down',
      })),
    },
  });
}

const sent = (calls: MockRequest[]) =>
  calls.map((c) => (c.body as { books: BookRef[] }).books.map((b) => b.path));

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe('loadWork', () => {
  it("batches two cards' books into one request, each book once", async () => {
    const calls = mockFetch({ 'POST /admin/books/works': worksRoute() });
    const card1 = Promise.all([
      loadWork({ library_id: 1, path: 'a' }),
      loadWork({ library_id: 1, path: 'none' }),
    ]);
    const card2 = Promise.all([
      loadWork({ library_id: 2, path: 'b' }),
      loadWork({ library_id: 1, path: 'a' }), // the same book on two cards: asked once
    ]);
    expect(await card1).toEqual([
      { id: 'w-a', final: true },
      { id: '', final: true },
    ]);
    expect(await card2).toEqual([
      { id: 'w-b', final: true },
      { id: 'w-a', final: true },
    ]);
    expect(sent(calls)).toEqual([['a', 'none', 'b']]);
  });

  it('splits more than a batch', async () => {
    const calls = mockFetch({ 'POST /admin/books/works': worksRoute() });
    await Promise.all(
      Array.from({ length: WORKS_LIMIT + 1 }, (_, i) => loadWork({ library_id: 1, path: `p${i}` })),
    );
    expect(sent(calls).map((b) => b.length)).toEqual([WORKS_LIMIT, 1]);
  });

  it('leaves only the book whose lookup failed unresolved, not a clean miss beside it', async () => {
    mockFetch({ 'POST /admin/books/works': worksRoute() });
    expect(
      await Promise.all([
        loadWork({ library_id: 1, path: 'a' }),
        loadWork({ library_id: 1, path: 'none' }),
        loadWork({ library_id: 1, path: 'down' }),
      ]),
    ).toEqual([
      { id: 'w-a', final: true },
      { id: '', final: true },
      { id: '', final: false },
    ]);
  });

  it('answers a refused request as unresolved, not an error', async () => {
    mockFetch({ 'POST /admin/books/works': { status: 502, body: { error: 'down' } } });
    expect(await loadWork({ library_id: 1, path: 'a' })).toEqual({ id: '', final: false });
  });

  it('drops an aborted request', async () => {
    const calls = mockFetch({ 'POST /admin/books/works': worksRoute() });
    const gone = new AbortController();
    const dropped = loadWork({ library_id: 1, path: 'gone' }, gone.signal);
    const kept = loadWork({ library_id: 1, path: 'a' });
    gone.abort();
    await expect(dropped).rejects.toBeDefined();
    expect(await kept).toEqual({ id: 'w-a', final: true });
    expect(sent(calls)).toEqual([['a']]);
  });
});

describe('bookWorkQuery', () => {
  it('never re-sends a resolved book or a clean miss, and re-asks a failed one only after a while', async () => {
    vi.useFakeTimers({ toFake: ['Date'] });
    const calls = mockFetch({ 'POST /admin/books/works': worksRoute() });
    const qc = new QueryClient();
    const books = ['a', 'none', 'down'].map((path) => adminBook({ path }));
    const ask = () => Promise.all(books.map((b) => qc.fetchQuery(bookWorkQuery(b))));

    expect((await ask()).map((a) => a.id)).toEqual(['w-a', '', '']);
    expect(sent(calls)).toEqual([['a', 'none', 'down']]);

    // Soon after (a remount, a window focus): nothing is sent.
    vi.advanceTimersByTime(WORKS_RETRY_MS - 1_000);
    await ask();
    expect(calls).toHaveLength(1);

    // Later, only the book that may have failed is asked about again.
    vi.advanceTimersByTime(2_000);
    await ask();
    expect(sent(calls)).toEqual([['a', 'none', 'down'], ['down']]);
  });
});
