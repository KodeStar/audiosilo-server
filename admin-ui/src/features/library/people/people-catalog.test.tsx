import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { setToken } from '@/api/token';
import type { AdminBook } from '@/api/types';
import { mockFetch, type MockRoute } from '@/test/fetch-mock';
import { libraries } from '@/test/fixtures';
import { adminBook, authors, books, facets, narrators } from '@/test/library-fixtures';
import { renderApp } from '@/test/render-app';
import { signedInRoutes } from '@/test/routes';

function routes(over: Record<string, MockRoute> = {}) {
  return signedInRoutes({
    'GET /admin/authors': { body: authors },
    'GET /admin/narrators': { body: narrators },
    // The Books list a tile opens.
    'GET /admin/books': { body: { books } },
    'GET /admin/books/facets': { body: facets() },
    ...over,
  });
}

beforeEach(() => setToken('stored'));

describe('authors', () => {
  it('shows a tile per author, most books first, and filters them', async () => {
    mockFetch(routes());
    renderApp('/library/authors');
    const grid = await screen.findByRole('list', { name: 'Authors' });
    const tiles = within(grid).getAllByRole('link');
    expect(tiles[0]).toHaveTextContent('Brandon Sanderson');
    expect(tiles[0]).toHaveTextContent('2 books · 91h');
    expect(tiles).toHaveLength(4);
    expect(screen.getByText('4 authors, most books first')).toBeInTheDocument();

    const user = userEvent.setup();
    await user.type(screen.getByRole('searchbox', { name: 'Filter authors' }), 'wells');
    expect(within(grid).getAllByRole('link')).toHaveLength(1);
    expect(within(grid).getByRole('link')).toHaveTextContent('Martha Wells');
    await user.clear(screen.getByRole('searchbox', { name: 'Filter authors' }));
    await user.type(screen.getByRole('searchbox', { name: 'Filter authors' }), 'austen');
    expect(screen.getByText('Nothing matches “austen”.')).toBeInTheDocument();
  });

  it('opens the Books list filtered to the author', async () => {
    mockFetch(routes());
    const { router } = renderApp('/library/authors');
    const grid = await screen.findByRole('list', { name: 'Authors' });
    await userEvent.setup().click(within(grid).getByRole('link', { name: /^Martha Wells/ }));
    await waitFor(() => expect(router.state.location.pathname).toBe('/library'));
    expect(router.state.location.search).toEqual({ author: 'Martha Wells' });
  });

  it('filters by library', async () => {
    const calls = mockFetch(routes());
    const { router } = renderApp('/library/authors');
    await screen.findByRole('list', { name: 'Authors' });
    const filter = screen.getByRole('group', { name: 'Library' });
    await userEvent.setup().click(within(filter).getByRole('button', { name: 'Kids' }));
    await waitFor(() => expect(router.state.location.search).toEqual({ library: 2 }));
    await waitFor(() =>
      expect(
        calls.some((c) => c.path === '/admin/authors' && c.query.get('library_id') === '2'),
      ).toBe(true),
    );
  });

  it('merges spellings, and undo puts each book back', async () => {
    // Two pages of books tagged with the other spelling: one never edited, one
    // with an edit already (which undo restores by setting the old spelling).
    const fresh = adminBook({ path: 'Sanderson/Elantris', author: 'Sanderson, Brandon' });
    const edited = adminBook({
      path: 'Sanderson/Warbreaker',
      author: 'Sanderson, Brandon',
      edited: true,
    });
    const calls = mockFetch(
      routes({
        'GET /admin/books': (req) => {
          if (req.query.get('author') !== 'Sanderson, Brandon') return { body: { books } };
          return req.query.get('cursor')
            ? { body: { books: [edited] } }
            : { body: { books: [fresh], next_cursor: 'p2' } };
        },
        'POST /admin/books/bulk': (req) => ({
          body: { updated: (req.body as { books: AdminBook[] }).books.length },
        }),
      }),
    );
    renderApp('/library/authors');
    expect(
      await screen.findByText('“Sanderson, Brandon” looks like Brandon Sanderson'),
    ).toBeInTheDocument();
    expect(
      screen.getByText(
        '1 book is tagged with the other spelling. Merging saves an edit on it; the files are untouched.',
      ),
    ).toBeInTheDocument();

    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: 'Merge authors' }));
    expect(await screen.findByText('Merged into Brandon Sanderson')).toBeInTheDocument();
    expect(
      screen.getByText('2 books updated and locked. Revert from each book page.'),
    ).toBeInTheDocument();
    const lookups = calls.filter((c) => c.path === '/admin/books' && c.query.get('author'));
    expect(lookups.map((c) => c.query.get('limit'))).toEqual(['200', '200']);
    const bulk = () => calls.filter((c) => c.path === '/admin/books/bulk').map((c) => c.body);
    expect(bulk()).toEqual([
      {
        books: [
          { library_id: 1, path: 'Sanderson/Elantris' },
          { library_id: 1, path: 'Sanderson/Warbreaker' },
        ],
        set: { author: 'Brandon Sanderson' },
      },
    ]);

    await user.click(screen.getByRole('button', { name: 'Undo' }));
    expect(await screen.findByText('Merge undone')).toBeInTheDocument();
    expect(bulk().slice(1)).toEqual([
      { books: [{ library_id: 1, path: 'Sanderson/Elantris' }], revert: ['author'] },
      {
        books: [{ library_id: 1, path: 'Sanderson/Warbreaker' }],
        set: { author: 'Sanderson, Brandon' },
      },
    ]);
  });

  it('says how to get authors when there are no books', async () => {
    mockFetch(
      routes({
        'GET /admin/authors': { body: { authors: [], merge_suggestions: [], unknown: 0 } },
        'GET /admin/libraries': {
          body: { libraries: libraries([{ book_count: 0 }, { book_count: 0 }]) },
        },
      }),
    );
    renderApp('/library/authors');
    expect(await screen.findByText('No authors yet')).toBeInTheDocument();
    expect(
      await screen.findByText('Authors show up here once a library has books.'),
    ).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Go to Libraries' })).toHaveAttribute(
      'href',
      '/admin/library/libraries',
    );
  });

  it('shows a failure with a retry', async () => {
    mockFetch(
      routes({ 'GET /admin/authors': { status: 500, body: { error: 'database locked' } } }),
    );
    renderApp('/library/authors');
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent("Authors didn't load");
    expect(alert).toHaveTextContent('database locked');
    expect(within(alert).getByRole('button', { name: 'Try again' })).toBeInTheDocument();
  });
});

describe('narrators', () => {
  it('shows a card per narrator, most hours first, and opens their books', async () => {
    mockFetch(routes());
    const { router } = renderApp('/library/narrators');
    const list = await screen.findByRole('list', { name: 'Narrators' });
    const cards = within(list).getAllByRole('link');
    expect(cards.map((c) => c.textContent)).toEqual([
      'Michael Kramer & Kate Reading2 books · 91h narrated',
      'Mel Hudson1 book · 16h 40m narrated',
      'Kevin R. Free1 book · 3h 18m narrated',
    ]);
    expect(screen.getByText('1 book has no narrator')).toBeInTheDocument();
    // No suggestions here: no merge notice.
    expect(screen.queryByRole('button', { name: 'Merge narrators' })).not.toBeInTheDocument();

    await userEvent.setup().click(cards[0]);
    await waitFor(() => expect(router.state.location.pathname).toBe('/library'));
    expect(router.state.location.search).toEqual({ narrator: 'Michael Kramer & Kate Reading' });
  });

  it('merges narrator spellings through the narrator field', async () => {
    const calls = mockFetch(
      routes({
        'GET /admin/narrators': {
          body: {
            ...narrators,
            narrators: [
              ...narrators.narrators,
              { name: 'Kramer, Michael & Reading, Kate', books: 1, duration: 3600 },
            ],
            merge_suggestions: [
              {
                names: ['Kramer, Michael & Reading, Kate', 'Michael Kramer & Kate Reading'],
                suggested: 'Michael Kramer & Kate Reading',
                books: 3,
              },
            ],
          },
        },
        'GET /admin/books': (req) =>
          req.query.get('narrator')
            ? {
                body: {
                  books: [adminBook({ path: 'x', narrator: 'Kramer, Michael & Reading, Kate' })],
                },
              }
            : { body: { books } },
        'POST /admin/books/bulk': { body: { updated: 1 } },
      }),
    );
    renderApp('/library/narrators');
    await userEvent.setup().click(await screen.findByRole('button', { name: 'Merge narrators' }));
    expect(
      await screen.findByText('Merged into Michael Kramer & Kate Reading'),
    ).toBeInTheDocument();
    expect(calls.find((c) => c.path === '/admin/books/bulk')?.body).toEqual({
      books: [{ library_id: 1, path: 'x' }],
      set: { narrator: 'Michael Kramer & Kate Reading' },
    });
  });
});
