import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { setToken } from '@/api/token';
import type { AdminBook } from '@/api/types';
import { mockFetch, type MockRequest, type MockRoute } from '@/test/fetch-mock';
import { fictionGrant, kidsShare, serverInfo } from '@/test/fixtures';
import { adminBook, bookDetail, books, facets } from '@/test/library-fixtures';
import { renderApp } from '@/test/render-app';
import { signedInRoutes } from '@/test/routes';

/** The full list's requests (the shelves ask for 12 books, the list for a page of 60). */
const isList = (c: MockRequest) => c.path === '/admin/books' && c.query.get('limit') === '60';

/**
 * The book list answers by what it's asked: the shelves' "no cover" and
 * "unmatched" queries get the one such book, everything else `list`.
 */
function bookRoutes(list: AdminBook[] = books, over: Record<string, MockRoute> = {}) {
  return signedInRoutes({
    'GET /admin/books': (req) => {
      if (req.query.get('has_cover') === 'false' && req.query.get('limit') === '12') {
        return { body: { books: list.filter((b) => !b.has_cover) } };
      }
      if (req.query.get('matched') === 'false' && req.query.get('limit') === '12') {
        return { body: { books: list.filter((b) => !b.matched) } };
      }
      return { body: { books: list } };
    },
    'GET /admin/books/facets': (req) =>
      req.query.get('added_after') && req.query.size === 1
        ? { body: facets({ total: 2 }) }
        : { body: facets({ total: list.length }) },
    'GET /admin/shares': { body: { shares: [kidsShare, fictionGrant] } },
    ...over,
  });
}

const allBooks = () => screen.findByRole('list', { name: 'All books' });

beforeEach(() => setToken('stored'));

describe('books: browsing', () => {
  it('shows the shelves above the full list', async () => {
    mockFetch(bookRoutes());
    renderApp('/library');
    const recent = await screen.findByRole('region', { name: 'Recently added' });
    expect(await within(recent).findByRole('link', { name: /Words of Radiance/ })).toBeVisible();
    expect(await screen.findByText('2 this week')).toBeInTheDocument();

    const curate = await screen.findByRole('region', { name: 'Continue curating' });
    expect(within(curate).getByText(/a minute of your attention/)).toBeInTheDocument();
    // The issue is part of the book's link: a click on it opens the book.
    expect((await within(curate).findByText('No cover')).closest('a')).toHaveAttribute(
      'href',
      expect.stringContaining('/library/book?'),
    );
    expect(within(curate).getAllByRole('listitem')).toHaveLength(1);

    expect(screen.getByRole('heading', { name: 'All books' })).toBeInTheDocument();
    const list = await allBooks();
    expect(await within(list).findAllByRole('listitem')).toHaveLength(4);
    // The flags: no cover and not matched on Murderbot, transcodes on the opus book.
    expect(within(list).getByRole('img', { name: 'Transcodes (opus)' })).toBeInTheDocument();
    expect(
      within(list).getByRole('img', { name: 'Not matched to community metadata' }),
    ).toBeInTheDocument();
  });

  it('leaves out what community metadata adds while it is off', async () => {
    const calls = mockFetch(
      bookRoutes(books, {
        'GET /server': {
          body: { ...serverInfo, capabilities: { ...serverInfo.capabilities, metadata: false } },
        },
      }),
    );
    renderApp('/library');
    const list = await allBooks();
    await within(list).findAllByRole('listitem');
    expect(within(list).queryByRole('img', { name: /community metadata/ })).toBeNull();
    expect(calls.some((c) => c.query.get('matched') === 'false')).toBe(false);
    await userEvent.setup().click(screen.getByRole('button', { name: /Filters/ }));
    const sheet = await screen.findByRole('dialog', { name: 'Filter books' });
    expect(within(sheet).queryByRole('region', { name: 'Community metadata' })).toBeNull();
  });

  it('filters by library, with each library counted', async () => {
    const calls = mockFetch(
      bookRoutes(books, {
        'GET /admin/libraries': {
          body: {
            libraries: [
              {
                id: 1,
                name: 'Fiction',
                root: '/f',
                default_view: '',
                sort_order: 0,
                book_count: 3,
                available: true,
                scan: { running: false, total: 0, done: 0, indexed: 0 },
              },
              {
                id: 2,
                name: 'Kids',
                root: '/k',
                default_view: '',
                sort_order: 1,
                book_count: 1,
                available: false,
                scan: { running: false, total: 0, done: 0, indexed: 0 },
              },
            ],
          },
        },
      }),
    );
    const { router } = renderApp('/library');
    const group = await screen.findByRole('group', { name: 'Library' });
    expect(await within(group).findByRole('button', { name: 'All 4' })).toHaveAttribute(
      'aria-pressed',
      'true',
    );
    expect(within(group).getByRole('button', { name: 'Fiction 3' })).toBeInTheDocument();
    await userEvent.setup().click(within(group).getByRole('button', { name: 'Kids offline' }));
    await waitFor(() => expect(router.state.location.search).toEqual({ library: 2 }));
    await waitFor(() =>
      expect(calls.some((c) => isList(c) && c.query.get('library_id') === '2')).toBe(true),
    );
    // A filtered list has no shelves, and says how many books match.
    expect(screen.queryByRole('region', { name: 'Recently added' })).toBeNull();
    expect(await screen.findByRole('heading', { name: '4 books' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Remove Library: Kids' })).toBeInTheDocument();
  });
});

describe('books: search, filters and sort', () => {
  it('searches as you type, and clears from the chip', async () => {
    const calls = mockFetch(bookRoutes());
    const { router } = renderApp('/library');
    const user = userEvent.setup();
    await user.type(await screen.findByRole('textbox', { name: 'Filter books' }), 'kings');
    await waitFor(() => expect(router.state.location.search).toEqual({ q: 'kings' }));
    expect(calls.some((c) => isList(c) && c.query.get('q') === 'kings')).toBe(true);
    // Debounced: no request for a half-typed word.
    expect(calls.some((c) => isList(c) && c.query.get('q') === 'k')).toBe(false);

    const chips = await screen.findByRole('list', { name: 'Active filters' });
    expect(within(chips).getByText('Matches “kings”')).toBeInTheDocument();
    await user.click(within(chips).getByRole('button', { name: 'Clear search' }));
    await waitFor(() => expect(router.state.location.search).toEqual({}));
    expect(screen.getByRole('textbox', { name: 'Filter books' })).toHaveValue('');
  });

  it('filters from the sheet, with counts, and shows the filter as a chip', async () => {
    const calls = mockFetch(bookRoutes());
    const { router } = renderApp('/library');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Filters' }));
    const sheet = await screen.findByRole('dialog', { name: 'Filter books' });
    const cover = within(sheet).getByRole('region', { name: 'Cover' });
    await user.click(within(cover).getByRole('button', { name: 'Missing cover 1' }));
    const format = within(sheet).getByRole('region', { name: 'Format' });
    await user.click(within(format).getByRole('button', { name: 'M4B 3' }));
    await user.click(within(format).getByRole('button', { name: 'OPUS 1' }));
    const length = within(sheet).getByRole('region', { name: 'Length' });
    await user.click(within(length).getByRole('button', { name: 'Over 30h' }));

    await waitFor(() =>
      expect(router.state.location.search).toEqual({
        cover: 'no',
        format: ['m4b', 'opus'],
        length: 'epic',
      }),
    );
    await waitFor(() => {
      const last = calls.filter(isList).at(-1)!;
      expect(last.query.get('has_cover')).toBe('false');
      expect(last.query.getAll('format')).toEqual(['m4b', 'opus']);
      expect(last.query.get('min_duration')).toBe(String(30 * 3600));
    });
    // Facet counts ask with the same filters.
    expect(calls.some((c) => c.path === '/admin/books/facets' && c.query.get('has_cover'))).toBe(
      true,
    );
    expect(within(cover).getByRole('button', { name: 'Missing cover 1' })).toHaveAttribute(
      'aria-pressed',
      'true',
    );
    await user.click(within(sheet).getByRole('button', { name: 'Show 4 books' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());

    expect(screen.getByRole('button', { name: /^Filters/ })).toHaveTextContent('4');
    const chips = screen.getByRole('list', { name: 'Active filters' });
    expect(within(chips).getByText('Missing cover')).toBeInTheDocument();
    expect(within(chips).getByText('Format: OPUS')).toBeInTheDocument();
    await user.click(within(chips).getByRole('button', { name: 'Remove Format: M4B' }));
    await waitFor(() =>
      expect(router.state.location.search).toEqual({
        cover: 'no',
        format: ['opus'],
        length: 'epic',
      }),
    );
    await user.click(within(chips).getByRole('button', { name: 'Clear all' }));
    await waitFor(() => expect(router.state.location.search).toEqual({}));
  });

  it('takes the exact author filter other screens link with', async () => {
    const calls = mockFetch(bookRoutes());
    const { router } = renderApp('/library?author=Brandon%20Sanderson');
    const chips = await screen.findByRole('list', { name: 'Active filters' });
    expect(within(chips).getByText('Author: Brandon Sanderson')).toBeInTheDocument();
    await waitFor(() =>
      expect(calls.some((c) => isList(c) && c.query.get('author') === 'Brandon Sanderson')).toBe(
        true,
      ),
    );
    await userEvent
      .setup()
      .click(within(chips).getByRole('button', { name: 'Remove Author: Brandon Sanderson' }));
    await waitFor(() => expect(router.state.location.search).toEqual({}));
  });

  it('sorts', async () => {
    const calls = mockFetch(bookRoutes());
    const { router } = renderApp('/library');
    await userEvent
      .setup()
      .selectOptions(await screen.findByRole('combobox', { name: 'Sort' }), 'Longest first');
    await waitFor(() => expect(router.state.location.search).toEqual({ sort: 'duration' }));
    await waitFor(() =>
      expect(
        calls.some(
          (c) => isList(c) && c.query.get('sort') === 'duration' && c.query.get('order') === 'desc',
        ),
      ).toBe(true),
    );
  });

  it('opens a series filter in reading order, and keeps a title sort chosen over it', async () => {
    const calls = mockFetch(bookRoutes());
    const { router } = renderApp('/library?series=The%20Stormlight%20Archive');
    const sort = await screen.findByRole('combobox', { name: 'Sort' });
    expect(sort).toHaveValue('series');
    await waitFor(() =>
      expect(
        calls.some(
          (c) => isList(c) && c.query.get('sort') === 'series' && c.query.get('order') === 'asc',
        ),
      ).toBe(true),
    );
    const user = userEvent.setup();
    // Series chosen over a series filter is kept when the filter goes.
    await user.selectOptions(sort, 'Recently added');
    await user.selectOptions(sort, 'Series');
    await waitFor(() =>
      expect(router.state.location.search).toEqual({
        series: 'The Stormlight Archive',
        sort: 'series',
      }),
    );
    await user.selectOptions(sort, 'Title');
    await waitFor(() =>
      expect(router.state.location.search).toEqual({
        series: 'The Stormlight Archive',
        sort: 'title',
      }),
    );
  });

  it("links a tile's author to their books, keeping the library", async () => {
    mockFetch(bookRoutes());
    const { router } = renderApp('/library?library=1&q=a');
    const list = await allBooks();
    await userEvent.setup().click(await within(list).findByRole('link', { name: 'Martha Wells' }));
    await waitFor(() =>
      expect(router.state.location.search).toEqual({ library: 1, author: 'Martha Wells' }),
    );
    expect(router.state.location.pathname).toBe('/library');
  });

  it('toggles the book from its author link while selecting', async () => {
    mockFetch(bookRoutes());
    const { router } = renderApp('/library?q=a');
    const user = userEvent.setup();
    const list = await allBooks();
    await user.click(
      await within(list).findByRole('checkbox', { name: 'Select The Way of Kings' }),
    );
    await user.click(within(list).getByRole('link', { name: 'Martha Wells' }));
    expect(screen.getByRole('toolbar', { name: 'Bulk actions' })).toHaveTextContent('2 selected');
    expect(router.state.location.search).toEqual({ q: 'a' });
  });

  it('loads the next page when the end of the list is near', async () => {
    const calls = mockFetch(
      bookRoutes(books, {
        'GET /admin/books': (req) =>
          req.query.get('cursor') === 'page2'
            ? { body: { books: [adminBook({ path: 'x/Elantris', title: 'Elantris' })] } }
            : { body: { books, next_cursor: 'page2' } },
      }),
    );
    renderApp('/library?q=a');
    const list = await allBooks();
    expect(await within(list).findByRole('link', { name: /Elantris/ })).toBeInTheDocument();
    expect(calls.filter((c) => isList(c) && c.query.get('cursor') === 'page2')).toHaveLength(1);
  });
});

describe('books: table', () => {
  it('lists the books with their status', async () => {
    mockFetch(bookRoutes());
    const { router } = renderApp('/library?view=table');
    const table = await screen.findByRole('table', { name: 'All books' });
    expect(within(table).getByRole('columnheader', { name: 'Metadata' })).toBeInTheDocument();
    const row = within(table)
      .getByRole('link', { name: 'All Systems Red' })
      .closest('tr') as HTMLElement;
    expect(within(row).getByText('Unmatched')).toBeInTheDocument();
    expect(within(row).getByText('3h 18m')).toBeInTheDocument();
    const opus = within(table)
      .getByRole('link', { name: 'Children of Time' })
      .closest('tr') as HTMLElement;
    expect(within(opus).getByText('Transcode')).toBeInTheDocument();
    const kings = within(table)
      .getByRole('link', { name: 'The Way of Kings' })
      .closest('tr') as HTMLElement;
    expect(within(kings).getByText('m4b ×4')).toBeInTheDocument();
    expect(within(kings).getByText('#1')).toBeInTheDocument();

    const user = userEvent.setup();
    // The author, narrator and series cells link to their books, not to the book.
    await user.click(within(row).getByRole('link', { name: 'Kevin R. Free' }));
    await waitFor(() =>
      expect(router.state.location.search).toEqual({ view: 'table', narrator: 'Kevin R. Free' }),
    );
    expect(router.state.location.pathname).toBe('/library');
    await user.click(screen.getByRole('button', { name: 'Remove Narrator: Kevin R. Free' }));
    await waitFor(() => expect(router.state.location.search).toEqual({ view: 'table' }));

    // The header checkbox selects every loaded row.
    await user.click(within(table).getByRole('checkbox', { name: 'Select all loaded books' }));
    expect(await screen.findByRole('toolbar', { name: 'Bulk actions' })).toHaveTextContent(
      '4 selected',
    );
    // While selecting, a cell's link toggles its row instead of filtering.
    await user.click(within(row).getByRole('link', { name: 'Kevin R. Free' }));
    expect(screen.getByRole('toolbar', { name: 'Bulk actions' })).toHaveTextContent('3 selected');
    expect(router.state.location.search).toEqual({ view: 'table' });

    // Grid again, from the view switch.
    await user.click(screen.getByRole('button', { name: 'Cover grid' }));
    await waitFor(() => expect(router.state.location.search).toEqual({}));
  });
});

describe('books: selection and bulk actions', () => {
  it('edits fields over the selection', async () => {
    const calls = mockFetch(
      bookRoutes(books, { 'POST /admin/books/bulk': { body: { updated: 2 } } }),
    );
    renderApp('/library');
    const user = userEvent.setup();
    const list = await allBooks();
    await user.click(
      await within(list).findByRole('checkbox', { name: 'Select The Way of Kings' }),
    );
    await user.click(within(list).getByRole('checkbox', { name: 'Select Words of Radiance' }));
    const bar = screen.getByRole('toolbar', { name: 'Bulk actions' });
    expect(bar).toHaveTextContent('2 selected');

    await user.click(within(bar).getByRole('button', { name: 'Edit fields' }));
    const dialog = await screen.findByRole('dialog', { name: 'Edit 2 books' });
    // A shared value is the placeholder.
    expect(within(dialog).getByLabelText('Author')).toHaveAttribute(
      'placeholder',
      'Brandon Sanderson',
    );
    const apply = within(dialog).getByRole('button', { name: 'Apply to 2 books' });
    expect(apply).toBeDisabled();
    await user.type(within(dialog).getByLabelText('Series'), ' Stormlight ');
    await user.click(apply);

    expect(await screen.findByText('Updated 2 books')).toBeInTheDocument();
    expect(calls.find((c) => c.path === '/admin/books/bulk')?.body).toEqual({
      books: [
        { library_id: 1, path: books[0].path },
        { library_id: 1, path: books[1].path },
      ],
      set: { series: 'Stormlight' },
    });
    // Done: the selection is cleared.
    await waitFor(() => expect(screen.queryByRole('toolbar', { name: 'Bulk actions' })).toBeNull());
  });

  it('says how many values differ, and shows a failed edit in the dialog', async () => {
    mockFetch(
      bookRoutes(books, {
        'POST /admin/books/bulk': { status: 400, body: { error: 'series: too long' } },
      }),
    );
    renderApp('/library');
    const user = userEvent.setup();
    const list = await allBooks();
    await user.click(
      await within(list).findByRole('checkbox', { name: 'Select The Way of Kings' }),
    );
    await user.click(within(list).getByRole('checkbox', { name: 'Select All Systems Red' }));
    await user.click(screen.getByRole('button', { name: 'Edit fields' }));
    const dialog = await screen.findByRole('dialog', { name: 'Edit 2 books' });
    expect(within(dialog).getByLabelText('Narrator')).toHaveAttribute(
      'placeholder',
      '2 different values. Leave blank to keep',
    );
    await user.type(within(dialog).getByLabelText('Series'), 'x');
    await user.click(within(dialog).getByRole('button', { name: 'Apply to 2 books' }));
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(
      'Nothing was changed. series: too long',
    );
  });

  it('adds the selection to a share', async () => {
    const calls = mockFetch(bookRoutes(books, { 'POST /admin/shares/7/paths': { status: 204 } }));
    renderApp('/library');
    const user = userEvent.setup();
    const list = await allBooks();
    await user.click(await within(list).findByRole('checkbox', { name: 'Select All Systems Red' }));
    await user.click(screen.getByRole('button', { name: 'Add to share' }));
    const dialog = await screen.findByRole('dialog', { name: 'Add All Systems Red to a share' });
    // Whole-library grants aren't offered.
    const cards = await within(dialog).findByRole('radiogroup', { name: 'Shares' });
    expect(within(cards).getAllByRole('radio')).toHaveLength(1);
    expect(within(cards).getByText('1 person · 1 folder')).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Add to share' }));
    expect(await screen.findByText('Added to Cosy mysteries')).toBeInTheDocument();
    // One request for the whole selection.
    expect(calls.filter((c) => c.path === '/admin/shares/7/paths').map((c) => c.body)).toEqual([
      { rules: [{ library_id: 1, path: 'Martha Wells/All Systems Red' }] },
    ]);
    await waitFor(() => expect(screen.queryByRole('toolbar', { name: 'Bulk actions' })).toBeNull());
  });

  it('points to People > Shares when there are no shares', async () => {
    mockFetch(bookRoutes(books, { 'GET /admin/shares': { body: { shares: [fictionGrant] } } }));
    renderApp('/library');
    const user = userEvent.setup();
    const list = await allBooks();
    await user.click(await within(list).findByRole('checkbox', { name: 'Select All Systems Red' }));
    await user.click(screen.getByRole('button', { name: 'Add to share' }));
    const dialog = await screen.findByRole('dialog', { name: 'Add All Systems Red to a share' });
    expect(await within(dialog).findByText('No shares yet')).toBeInTheDocument();
    expect(within(dialog).getByRole('link', { name: 'Go to Shares' })).toHaveAttribute(
      'href',
      '/admin/people/shares',
    );
  });

  it('toggles a tile while selecting, opens the book otherwise, and clears on Esc', async () => {
    mockFetch(
      bookRoutes(books, {
        'GET /admin/libraries/1/book': { body: bookDetail({ book: books[2] }) },
      }),
    );
    const { router } = renderApp('/library');
    const user = userEvent.setup();
    const list = await allBooks();
    await user.click(
      await within(list).findByRole('checkbox', { name: 'Select The Way of Kings' }),
    );
    // While selecting, clicking a tile adds it instead of opening it.
    await user.click(
      within(list).getByRole('link', { name: 'Words of Radiance by Brandon Sanderson' }),
    );
    expect(screen.getByRole('toolbar', { name: 'Bulk actions' })).toHaveTextContent('2 selected');
    expect(within(list).getByRole('checkbox', { name: 'Select Words of Radiance' })).toBeChecked();

    await user.keyboard('{Escape}');
    await waitFor(() => expect(screen.queryByRole('toolbar', { name: 'Bulk actions' })).toBeNull());

    await user.click(within(list).getByRole('link', { name: 'All Systems Red by Martha Wells' }));
    await waitFor(() => expect(router.state.location.pathname).toBe('/library/book'));
    expect(router.state.location.search).toEqual({
      library: 1,
      path: 'Martha Wells/All Systems Red',
    });
  });
});

describe('books: states', () => {
  it('points to Libraries when there are no books at all', async () => {
    mockFetch(bookRoutes([]));
    renderApp('/library');
    expect(await screen.findByText('No books yet')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Open Libraries' })).toHaveAttribute(
      'href',
      '/admin/library/libraries',
    );
    expect(screen.queryByRole('textbox', { name: 'Filter books' })).toBeNull();
  });

  it('says when nothing matches, and clears the filters', async () => {
    mockFetch(bookRoutes([]));
    const { router } = renderApp('/library?q=zzz&cover=no');
    expect(await screen.findByText('No books match these filters')).toBeInTheDocument();
    await userEvent.setup().click(screen.getByRole('button', { name: 'Clear filters' }));
    await waitFor(() => expect(router.state.location.search).toEqual({}));
  });

  it('shows a failed load with a retry', async () => {
    let fail = true;
    mockFetch(
      bookRoutes(books, {
        'GET /admin/books': () =>
          fail ? { status: 500, body: { error: 'database is locked' } } : { body: { books } },
      }),
    );
    renderApp('/library?q=a');
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent("The books couldn't be loaded");
    expect(alert).toHaveTextContent('database is locked');
    fail = false;
    await userEvent.setup().click(within(alert).getByRole('button', { name: 'Try again' }));
    expect(await within(await allBooks()).findAllByRole('listitem')).toHaveLength(4);
  });
});
