import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { setToken } from '@/api/token';
import type { AdminBook, DuplicateGroup } from '@/api/types';
import { mockFetch, type MockReply, type MockRoute } from '@/test/fetch-mock';
import { issuesSummary, libraries } from '@/test/fixtures';
import { adminBook } from '@/test/library-fixtures';
import { renderApp } from '@/test/render-app';
import { signedInRoutes } from '@/test/routes';

// Health > Issues: categories, the triage queue, ignore with Undo, fixes,
// duplicates and the offline safety stop.

const broken = adminBook({
  path: 'Terry Pratchett/Guards! Guards!',
  title: 'Guards! Guards!',
  author: 'Terry Pratchett',
  scan_error: 'empty_file',
  scan_error_file: 'Terry Pratchett/Guards! Guards!/07.mp3',
});
const coverless = [
  adminBook({ path: 'A/One', title: 'Book One', has_cover: false }),
  adminBook({ path: 'A/Two', title: 'Book Two', has_cover: false }),
];

// A book ripped to disc folders, listed by its first disc.
const disc = adminBook({ path: 'Cowell/Dragonese/CD1', title: 'Dragonese' });

/** The issues with one book split across disc folders. */
function splitSummary() {
  const summary = issuesSummary();
  summary.categories.splice(2, 0, { kind: 'split_discs', count: 1, ignored: 0, samples: [] });
  return summary;
}

/** A book page as POST .../book/rescan answers it. */
function rescanned(book: AdminBook) {
  return {
    book,
    fields: {},
    chapters: [],
    files: [],
    listeners: [],
    shares: [],
    folder: { path: '', override: '' },
    description: '',
    indexed_at: '',
  };
}

function routes(over: Record<string, MockRoute> = {}) {
  return signedInRoutes({
    'GET /admin/books': (req) => {
      const issue = req.query.get('issue');
      if (issue === 'scan_error') return { body: { books: [broken] } };
      if (issue === 'no_cover') {
        return { body: { books: req.query.get('issue_ignored') ? [] : coverless } };
      }
      return { body: { books: [] } };
    },
    ...over,
  });
}

beforeEach(() => setToken('stored'));
afterEach(() => vi.unstubAllGlobals());

describe('library health', () => {
  it('shows each category with its count and opens the first that needs attention', async () => {
    mockFetch(routes());
    renderApp('/health');
    expect(await screen.findByRole('heading', { name: 'Library health' })).toBeInTheDocument();
    expect(
      await screen.findByText('4 things could be better. None of it loses data.'),
    ).toBeInTheDocument();
    const card = screen.getByRole('button', { name: /Missing covers/ });
    expect(within(card).getByText('2')).toBeInTheDocument();
    expect(within(card).getByText('1 ignored')).toBeInTheDocument();
    // The first category with something in it is open: its book and why.
    expect(screen.getByRole('button', { name: /Files that couldn't be read/ })).toHaveAttribute(
      'aria-pressed',
      'true',
    );
    expect(await screen.findByText('07.mp3 is empty (0 bytes)')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Guards! Guards!' })).toBeInTheDocument();
  });

  it('ignores a book with an undo, and lists the ignored ones on request', async () => {
    const calls = mockFetch(
      routes({
        'POST /admin/issues/ignore': { status: 204 },
        'DELETE /admin/issues/ignore': { status: 204 },
      }),
    );
    const user = userEvent.setup();
    const { router } = renderApp('/health?issue=no_cover');
    const row = (await screen.findByText('Book One')).closest('li')!;
    await user.click(within(row).getByRole('button', { name: 'Ignore' }));
    await waitFor(() =>
      expect(
        calls.find((c) => c.method === 'POST' && c.path === '/admin/issues/ignore')?.body,
      ).toEqual({
        kind: 'no_cover',
        books: [{ library_id: 1, path: 'A/One' }],
      }),
    );
    expect(await screen.findByText('Ignored Book One')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Undo' }));
    await waitFor(() =>
      expect(calls.some((c) => c.method === 'DELETE' && c.path === '/admin/issues/ignore')).toBe(
        true,
      ),
    );

    await user.click(screen.getByRole('button', { name: 'Show ignored (1)' }));
    await waitFor(() =>
      expect(router.state.location.search).toMatchObject({ issue: 'no_cover', ignored: true }),
    );
    expect(await screen.findByText('Nothing ignored')).toBeInTheDocument();
    expect(calls.some((c) => c.query.get('issue_ignored') === 'true')).toBe(true);
  });

  it('ignores a selection from the floating bar', async () => {
    const calls = mockFetch(routes({ 'POST /admin/issues/ignore': { status: 204 } }));
    const user = userEvent.setup();
    renderApp('/health?issue=no_cover');
    await screen.findByText('Book One');
    await user.click(screen.getByRole('button', { name: 'Select all' }));
    const bar = screen.getByRole('toolbar', { name: 'Actions for the selected books' });
    expect(within(bar).getByText('2 selected')).toBeInTheDocument();
    await user.click(within(bar).getByRole('button', { name: 'Ignore' }));
    await waitFor(() =>
      expect(
        calls.find((c) => c.method === 'POST' && c.path === '/admin/issues/ignore')?.body,
      ).toMatchObject({
        kind: 'no_cover',
        books: [
          { library_id: 1, path: 'A/One' },
          { library_id: 1, path: 'A/Two' },
        ],
      }),
    );
  });

  it('reads an unreadable book again and says whether that fixed it', async () => {
    const calls = mockFetch(
      routes({
        'POST /admin/libraries/1/book/rescan': {
          body: {
            book: { ...broken, scan_error: undefined },
            fields: {},
            chapters: [],
            files: [],
            listeners: [],
            shares: [],
            folder: { path: '', override: '' },
            description: '',
            indexed_at: '',
          },
        },
      }),
    );
    const user = userEvent.setup();
    renderApp('/health?issue=scan_error');
    await user.click(await screen.findByRole('button', { name: 'Read again' }));
    expect(await screen.findByText('Guards! Guards! reads fine now')).toBeInTheDocument();
    expect(calls.find((c) => c.path === '/admin/libraries/1/book/rescan')?.query.get('path')).toBe(
      'Terry Pratchett/Guards! Guards!',
    );
  });

  it('switches a selection to the community’s detailed chapters', async () => {
    const coarse = [
      adminBook({
        path: 'Fry/Mythos',
        title: 'Mythos',
        chapter_count: 34,
        chapters_check: 'refine',
      }),
      adminBook({
        path: 'Fry/Heroes',
        title: 'Heroes',
        chapter_count: 20,
        chapters_check: 'refine',
      }),
    ];
    const summary = issuesSummary();
    summary.categories.push({ kind: 'detailed_chapters', count: 2, ignored: 0, samples: [] });
    const calls = mockFetch(
      routes({
        'GET /admin/issues': { body: summary },
        'GET /admin/books': (req) => ({
          body: { books: req.query.get('issue') === 'detailed_chapters' ? coarse : [] },
        }),
        'POST /admin/books/bulk': { body: { updated: 2 } },
      }),
    );
    const user = userEvent.setup();
    renderApp('/health?issue=detailed_chapters');
    expect(
      await screen.findByText('Has 34 chapters; the community’s finer ones fit this copy'),
    ).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Select all' }));
    const bar = await screen.findByRole('toolbar');
    await user.click(within(bar).getByRole('button', { name: 'Use detailed chapters' }));
    expect(await screen.findByText('Using detailed chapters for 2 books')).toBeInTheDocument();
    const bulk = calls.find((c) => c.path === '/admin/books/bulk');
    expect(bulk?.body).toEqual({
      books: coarse.map((b) => ({ library_id: b.library_id, path: b.path })),
      chapter_source: 'community',
    });
  });

  it('joins a book split across disc folders into one', async () => {
    const calls = mockFetch(
      routes({
        'GET /admin/issues': { body: splitSummary() },
        'GET /admin/books': (req) =>
          req.query.get('issue') === 'split_discs'
            ? { body: { books: [disc] } }
            : { body: { books: [] } },
        'PUT /admin/libraries/1/folder-override': { body: { status: 'override set' } },
      }),
    );
    const user = userEvent.setup();
    renderApp('/health?issue=split_discs');
    expect(
      await screen.findByText('Dragonese reads as one book per disc folder'),
    ).toBeInTheDocument();
    expect(
      screen.getByRole('button', { name: /One book split into disc folders/ }),
    ).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Join into one book' }));
    expect(await screen.findByText('Joining the discs of Dragonese')).toBeInTheDocument();
    const put = calls.find((c) => c.method === 'PUT');
    expect(put?.query.get('path')).toBe('Cowell/Dragonese');
    expect(put?.body).toEqual({ mode: 'book' });
    // The issues are fetched again once the folder is set.
    await waitFor(() =>
      expect(calls.filter((c) => c.path === '/admin/issues').length).toBeGreaterThan(1),
    );
  });

  it('sends one join while the first is in flight', async () => {
    let answer: (reply: MockReply) => void = () => {};
    const calls = mockFetch(
      routes({
        'GET /admin/issues': { body: splitSummary() },
        'GET /admin/books': (req) =>
          req.query.get('issue') === 'split_discs'
            ? { body: { books: [disc] } }
            : { body: { books: [] } },
        'PUT /admin/libraries/1/folder-override': () =>
          new Promise<MockReply>((resolve) => (answer = resolve)),
      }),
    );
    const user = userEvent.setup();
    renderApp('/health?issue=split_discs');
    const join = await screen.findByRole('button', { name: 'Join into one book' });
    await user.click(join);
    await waitFor(() => expect(join).toBeDisabled());
    await user.click(join);
    expect(calls.filter((c) => c.method === 'PUT')).toHaveLength(1);
    answer({ body: { status: 'override set' } });
    expect(await screen.findByText('Joining the discs of Dragonese')).toBeInTheDocument();
    await waitFor(() => expect(join).toBeEnabled());
    expect(calls.filter((c) => c.method === 'PUT')).toHaveLength(1);
  });

  it('says when joining the discs failed', async () => {
    mockFetch(
      routes({
        'GET /admin/issues': { body: splitSummary() },
        'GET /admin/books': (req) =>
          req.query.get('issue') === 'split_discs'
            ? { body: { books: [disc] } }
            : { body: { books: [] } },
        'PUT /admin/libraries/1/folder-override': { status: 500, body: { error: 'disk full' } },
      }),
    );
    renderApp('/health?issue=split_discs');
    await userEvent
      .setup()
      .click(await screen.findByRole('button', { name: 'Join into one book' }));
    expect(await screen.findByText("Couldn't join the discs of Dragonese")).toBeInTheDocument();
  });

  it('drops a book the re-read fixed from its list', async () => {
    let fixed = false;
    mockFetch(
      routes({
        'GET /admin/books': (req) =>
          req.query.get('issue') === 'scan_error' && !fixed
            ? { body: { books: [broken] } }
            : { body: { books: [] } },
        'POST /admin/libraries/1/book/rescan': () => {
          fixed = true;
          return { body: rescanned({ ...broken, scan_error: undefined }) };
        },
      }),
    );
    const user = userEvent.setup();
    renderApp('/health?issue=scan_error');
    await user.click(await screen.findByRole('button', { name: 'Read again' }));
    expect(await screen.findByText('Guards! Guards! reads fine now')).toBeInTheDocument();
    // The list is fetched again, not patched in place with a book it no longer holds.
    expect(await screen.findByText('All clear')).toBeInTheDocument();
    expect(screen.queryByRole('link', { name: 'Guards! Guards!' })).not.toBeInTheDocument();
  });

  it('reads a selection again a couple of books at a time', async () => {
    const many = Array.from({ length: 5 }, (_, i) =>
      adminBook({ ...broken, path: `Broken/${i}`, title: `Broken ${i}` }),
    );
    let inFlight = 0;
    let most = 0;
    const calls = mockFetch(
      routes({
        'GET /admin/books': (req) =>
          req.query.get('issue') === 'scan_error'
            ? { body: { books: many } }
            : { body: { books: [] } },
        'POST /admin/libraries/1/book/rescan': async (req) => {
          most = Math.max(most, ++inFlight);
          await new Promise((r) => setTimeout(r, 20));
          inFlight--;
          const b = many.find((m) => m.path === req.query.get('path'))!;
          return { body: rescanned({ ...b, scan_error: undefined }) };
        },
      }),
    );
    const user = userEvent.setup();
    renderApp('/health?issue=scan_error');
    await screen.findByText('Broken 0');
    await user.click(screen.getByRole('button', { name: 'Select all' }));
    const bar = screen.getByRole('toolbar', { name: 'Actions for the selected books' });
    await user.click(within(bar).getByRole('button', { name: 'Read again' }));
    await waitFor(() =>
      expect(calls.filter((c) => c.path === '/admin/libraries/1/book/rescan')).toHaveLength(5),
    );
    await waitFor(() => expect(inFlight).toBe(0));
    expect(most).toBeLessThanOrEqual(2);
  });

  it('keeps the category it opened on its own once it is cleared', async () => {
    let ignored = false;
    const summary = () =>
      issuesSummary({
        categories: issuesSummary().categories.map((c) =>
          c.kind === 'scan_error' ? { ...c, count: ignored ? 0 : 1, ignored: ignored ? 1 : 0 } : c,
        ),
      });
    mockFetch(
      routes({
        'GET /admin/issues': () => ({ body: summary() }),
        'GET /admin/books': (req) =>
          req.query.get('issue') === 'scan_error' && !ignored
            ? { body: { books: [broken] } }
            : { body: { books: [] } },
        'POST /admin/issues/ignore': () => {
          ignored = true;
          return { status: 204 };
        },
      }),
    );
    const user = userEvent.setup();
    renderApp('/health');
    const row = (await screen.findByRole('link', { name: 'Guards! Guards!' })).closest('li')!;
    await user.click(within(row).getByRole('button', { name: 'Ignore' }));
    // It stays on the category just cleared, rather than jumping to the next one.
    expect(await screen.findByText('All clear')).toBeInTheDocument();
    expect(
      screen.getByRole('heading', { name: "Files that couldn't be read" }),
    ).toBeInTheDocument();
  });

  it('compares duplicates side by side and stops suggesting ones that differ', async () => {
    const group: DuplicateGroup = {
      reason: 'same_book',
      ignored: false,
      books: [
        { ...adminBook({ path: 'Dune', title: 'Dune', format: 'm4b' }), listeners: 2 },
        {
          ...adminBook({ path: 'Dune (mp3)', title: 'Dune', format: 'mp3', file_count: 12 }),
          listeners: 0,
        },
      ],
    };
    const calls = mockFetch(
      routes({
        'GET /admin/issues/duplicates': { body: { groups: [group] } },
        'POST /admin/issues/ignore': { status: 204 },
      }),
    );
    const user = userEvent.setup();
    renderApp('/health?issue=duplicate');
    const card = (await screen.findByRole('heading', { name: 'Dune' })).closest('section')!;
    expect(within(card).getByText('Worth keeping')).toBeInTheDocument();
    expect(within(card).getByText('Another copy')).toBeInTheDocument();
    expect(within(card).getByText(/same title, author and length/)).toBeInTheDocument();
    await user.click(within(card).getByRole('button', { name: "They're different books" }));
    await waitFor(() =>
      expect(calls.find((c) => c.path === '/admin/issues/ignore')?.body).toEqual({
        kind: 'duplicate',
        books: [
          { library_id: 1, path: 'Dune' },
          { library_id: 1, path: 'Dune (mp3)' },
        ],
      }),
    );
  });

  it('celebrates an offline library as a safety stop, with a retry', async () => {
    const calls = mockFetch(
      routes({
        'GET /admin/issues': {
          body: issuesSummary({
            offline: [
              { library_id: 2, name: 'Kids', root: '/mnt/nas/kids', books: 849, listeners: 3 },
            ],
          }),
        },
        'GET /admin/libraries': { body: { libraries: libraries([{}, { available: false }]) } },
        'POST /admin/libraries/2/scan': { status: 202, body: { status: 'scan started' } },
      }),
    );
    const user = userEvent.setup();
    renderApp('/health');
    expect(await screen.findByText('Kids is offline. Nothing was deleted.')).toBeInTheDocument();
    expect(screen.getByText(/kept all 849 books/)).toBeInTheDocument();
    expect(screen.getByText(/Progress for 3 listeners is safe/)).toBeInTheDocument();
    const notice = screen
      .getByText('Kids is offline. Nothing was deleted.')
      .closest('div')!.parentElement!;
    await user.click(within(notice).getByRole('button', { name: 'Retry' }));
    await waitFor(() => expect(calls.some((c) => c.path === '/admin/libraries/2/scan')).toBe(true));
  });

  it('acts only on the selected books still listed', async () => {
    let ignoredOne = false;
    const calls = mockFetch(
      routes({
        'GET /admin/books': (req) =>
          req.query.get('issue') === 'no_cover' && !req.query.get('issue_ignored')
            ? { body: { books: ignoredOne ? coverless.slice(1) : coverless } }
            : { body: { books: [] } },
        'POST /admin/issues/ignore': () => {
          ignoredOne = true;
          return { status: 204 };
        },
      }),
    );
    const user = userEvent.setup();
    renderApp('/health?issue=no_cover');
    await screen.findByText('Book One');
    await user.click(screen.getByRole('button', { name: 'Select all' }));
    // Ignore Book One on its own row: it leaves the list but was selected.
    const row = screen.getByRole('link', { name: 'Book One' }).closest('li')!;
    await user.click(within(row).getByRole('button', { name: 'Ignore' }));
    await waitFor(() =>
      expect(screen.queryByRole('link', { name: 'Book One' })).not.toBeInTheDocument(),
    );
    const bar = screen.getByRole('toolbar', { name: 'Actions for the selected books' });
    expect(within(bar).getByText('1 selected')).toBeInTheDocument();
    await user.click(within(bar).getByRole('button', { name: 'Ignore' }));
    await waitFor(() =>
      expect(calls.filter((c) => c.path === '/admin/issues/ignore').at(-1)?.body).toEqual({
        kind: 'no_cover',
        books: [{ library_id: 1, path: 'A/Two' }],
      }),
    );
  });
});
