import { renderHook, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { keys, useScanWatcher } from '@/api/hooks';
import { setToken } from '@/api/token';
import { mockFetch, type MockRoute } from '@/test/fetch-mock';
import { idle, issuesSummary, libraries } from '@/test/fixtures';
import type { MetadataSource } from '@/api/types';
import { bookDetail } from '@/test/library-fixtures';
import { renderApp } from '@/test/render-app';
import { signedInRoutes } from '@/test/routes';

// Phase 3 outside the Health screens: library scan settings, the Overview's
// "needs attention" card, the book page's rescan and match link, and the scan
// watcher treating a queued scan as in progress.

beforeEach(() => setToken('stored'));
afterEach(() => vi.unstubAllGlobals());

function libraryRoutes(over: Record<string, MockRoute> = {}) {
  return signedInRoutes({
    'GET /libraries/1/books': { body: { books: [] } },
    'GET /libraries/2/books': { body: { books: [] } },
    ...over,
  });
}

describe('library scan settings', () => {
  async function openEdit() {
    const user = userEvent.setup();
    const card = await screen.findByRole('listitem', { name: 'Fiction' });
    await user.click(within(card).getByRole('button', { name: 'Actions for Fiction' }));
    await user.click(await screen.findByRole('menuitem', { name: 'Edit library...' }));
    return { user, dialog: await screen.findByRole('dialog', { name: 'Edit Fiction' }) };
  }

  it('saves a daily schedule without a rescan, and new skip rules with one', async () => {
    const calls = mockFetch(
      libraryRoutes({
        // Like the server: a scan is queued (and returned) when the skip rules change.
        'PATCH /admin/libraries/1': (req) => {
          const body = req.body as { ignore_patterns: string[] };
          const job = body.ignore_patterns.length ? { id: 9, trigger: 'change' } : undefined;
          return { body: { ...libraries()[0], ...body, job } };
        },
      }),
    );
    renderApp('/library/libraries');
    const { user, dialog } = await openEdit();
    await user.selectOptions(within(dialog).getByLabelText('Scan automatically'), 'daily');
    const time = within(dialog).getByLabelText('Time');
    await user.clear(time);
    await user.type(time, '04:30');
    await user.click(within(dialog).getByRole('button', { name: 'Save changes' }));
    await waitFor(() =>
      expect(calls.find((c) => c.method === 'PATCH')?.body).toMatchObject({
        scan_schedule: 'daily:04:30',
        ignore_patterns: [],
      }),
    );
    // Only the schedule changed: no "rescanning" promise in the toast.
    expect(await screen.findByText('Saved Fiction')).toBeInTheDocument();
    expect(screen.queryByText('AudioSilo is rescanning it now.')).not.toBeInTheDocument();

    const again = await openEdit();
    await again.user.type(again.dialog.querySelector('textarea')!, '*.sample.mp3{enter}Extras/');
    await again.user.click(within(again.dialog).getByRole('button', { name: 'Save changes' }));
    await waitFor(() =>
      expect(calls.filter((c) => c.method === 'PATCH')[1]?.body).toMatchObject({
        ignore_patterns: ['*.sample.mp3', 'Extras/'],
      }),
    );
    expect(await screen.findByText('AudioSilo is rescanning it now.')).toBeInTheDocument();
  });

  it('takes book details from folder names without a rescan', async () => {
    let source: MetadataSource = 'tags';
    const calls = mockFetch(
      libraryRoutes({
        'GET /admin/libraries': () => ({
          body: { libraries: libraries([{ metadata_source: source }]) },
        }),
        'PATCH /admin/libraries/1': (req) => {
          source = (req.body as { metadata_source: MetadataSource }).metadata_source;
          return { body: { ...libraries()[0], ...(req.body as object) } };
        },
      }),
    );
    renderApp('/library/libraries');
    const { user, dialog } = await openEdit();
    const select = within(dialog).getByLabelText('Book details come from');
    expect(select).toHaveValue('tags');
    await user.selectOptions(select, 'path');
    await user.click(within(dialog).getByRole('button', { name: 'Save changes' }));
    await waitFor(() =>
      expect(calls.find((c) => c.method === 'PATCH')?.body).toMatchObject({
        metadata_source: 'path',
      }),
    );
    expect(await screen.findByText('Saved Fiction')).toBeInTheDocument();
    expect(screen.queryByText('AudioSilo is rescanning it now.')).not.toBeInTheDocument();
    const card = await screen.findByRole('listitem', { name: 'Fiction' });
    expect(await within(card).findByText('Details from folder names')).toBeInTheDocument();
  });

  it('puts a refused pattern under the skip field', async () => {
    mockFetch(
      libraryRoutes({
        'PATCH /admin/libraries/1': {
          status: 400,
          body: {
            error: 'invalid ignore pattern: "[x": syntax error in pattern',
            code: 'invalid_pattern',
          },
        },
      }),
    );
    renderApp('/library/libraries');
    const { user, dialog } = await openEdit();
    await user.type(dialog.querySelector('textarea')!, '[[x');
    await user.click(within(dialog).getByRole('button', { name: 'Save changes' }));
    const field = within(dialog).getByLabelText('Skip these files and folders');
    await waitFor(() => expect(field).toHaveAttribute('aria-invalid', 'true'));
    expect(within(dialog).getByRole('alert')).toHaveTextContent('syntax error in pattern');
  });

  it('shows a queued scan as waiting, and the next scheduled scan', async () => {
    mockFetch(
      libraryRoutes({
        'GET /admin/libraries': {
          body: {
            libraries: libraries([
              {
                scan_schedule: 'every:6h',
                next_scan_at: new Date(Date.now() + 5 * 3600_000).toISOString(),
              },
              { scan: { ...idle, queued: true } },
            ]),
          },
        },
      }),
    );
    renderApp('/library/libraries');
    const fiction = await screen.findByRole('listitem', { name: 'Fiction' });
    expect(within(fiction).getByText(/Every 6 hours · next in 5 hr/)).toBeInTheDocument();
    const kids = screen.getByRole('listitem', { name: 'Kids' });
    expect(within(kids).getByText('Waiting')).toBeInTheDocument();
    expect(within(kids).getByRole('button', { name: 'Queued' })).toBeDisabled();
  });
});

it('lists what needs attention on the overview', async () => {
  mockFetch(signedInRoutes());
  renderApp('/');
  const card = (await screen.findByRole('heading', { name: 'Needs attention' })).closest(
    'section',
  )!;
  const link = await within(card).findByRole('link', { name: /Missing covers/ });
  expect(link).toHaveAttribute('href', '/admin/health?issue=no_cover');
  expect(within(link).getByText('2')).toBeInTheDocument();
  // A category with nothing in it isn't listed.
  expect(within(card).queryByText('Long books without chapters')).not.toBeInTheDocument();
});

it('says all is well on the overview when nothing needs attention', async () => {
  const clear = issuesSummary();
  clear.categories = clear.categories.map((c) => ({ ...c, count: 0 }));
  mockFetch(signedInRoutes({ 'GET /admin/issues': { body: clear } }));
  renderApp('/');
  expect(await screen.findByText('Nothing needs attention.')).toBeInTheDocument();
});

describe('book page', () => {
  const detail = bookDetail();
  const url = `/library/book?library=1&path=${encodeURIComponent(detail.book.path)}`;

  it('reads the files again from the more menu', async () => {
    const calls = mockFetch(
      signedInRoutes({
        'GET /admin/libraries/1/book': { body: detail },
        'POST /admin/libraries/1/book/rescan': { body: detail },
      }),
    );
    const user = userEvent.setup();
    renderApp(url);
    await user.click(await screen.findByRole('button', { name: 'More book actions' }));
    await user.click(await screen.findByRole('menuitem', { name: 'Read the files again' }));
    expect(await screen.findByText("Read the book's files again")).toBeInTheDocument();
    expect(calls.find((c) => c.path === '/admin/libraries/1/book/rescan')?.query.get('path')).toBe(
      detail.book.path,
    );
  });

  it('opens the match dialog from a ?match=1 link, and drops the param on close', async () => {
    mockFetch(
      signedInRoutes({
        'GET /admin/libraries/1/book': { body: detail },
        'GET /admin/libraries/1/book/match': { body: { candidates: [] } },
      }),
    );
    const user = userEvent.setup();
    const { router } = renderApp(`${url}&match=1`);
    const dialog = await screen.findByRole('dialog', { name: 'Match with community metadata' });
    await user.keyboard('{Escape}');
    await waitFor(() => expect(dialog).not.toBeInTheDocument());
    await waitFor(() => expect(router.state.location.search).not.toHaveProperty('match'));
  });
});

it('treats a queued scan as in progress, refreshing issues when it ends', async () => {
  let queued = true;
  mockFetch({
    'GET /admin/libraries': () => ({
      body: {
        libraries: libraries().map((l) => (l.id === 1 ? { ...l, scan: { ...idle, queued } } : l)),
      },
    }),
  });
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  qc.setQueryData(keys.issueSummary, issuesSummary());
  renderHook(() => useScanWatcher(), {
    wrapper: ({ children }) => <QueryClientProvider client={qc}>{children}</QueryClientProvider>,
  });
  await waitFor(() => expect(qc.getQueryData(keys.libraries)).toBeDefined());
  expect(qc.getQueryState(keys.issueSummary)?.isInvalidated).toBe(false);
  queued = false;
  await qc.refetchQueries({ queryKey: keys.libraries });
  await waitFor(() => expect(qc.getQueryState(keys.issueSummary)?.isInvalidated).toBe(true));
});
