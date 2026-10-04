import { renderHook, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { mockFetch } from '@/test/fetch-mock';
import { libraries } from '@/test/fixtures';
import { keys, useScanWatcher } from './hooks';

// When a scan finishes, everything it can change refetches: lists, counts,
// folder listings and any open book page (a rescan re-reads tags and chapters).

afterEach(() => vi.unstubAllGlobals());

it('refreshes open book pages when a library scan finishes', async () => {
  let running = true;
  mockFetch({
    'GET /admin/libraries': () => ({
      body: {
        libraries: libraries().map((l) =>
          l.id === 1 ? { ...l, scan: { ...l.scan, running } } : l,
        ),
      },
    }),
  });
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const bookPage = keys.book(1, 'Author/Book');
  qc.setQueryData(bookPage, { book: {} });
  const otherLibrary = keys.book(2, 'Other/Book');
  qc.setQueryData(otherLibrary, { book: {} });
  renderHook(() => useScanWatcher(), {
    wrapper: ({ children }) => <QueryClientProvider client={qc}>{children}</QueryClientProvider>,
  });
  await waitFor(() => expect(qc.getQueryData(keys.libraries)).toBeDefined());
  expect(qc.getQueryState(bookPage)?.isInvalidated).toBe(false);

  running = false;
  await qc.refetchQueries({ queryKey: keys.libraries });
  await waitFor(() => expect(qc.getQueryState(bookPage)?.isInvalidated).toBe(true));
  // Another library's pages are left alone.
  expect(qc.getQueryState(otherLibrary)?.isInvalidated).toBe(false);
});
