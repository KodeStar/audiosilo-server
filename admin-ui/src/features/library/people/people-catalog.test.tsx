import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { setToken } from '@/api/token';
import { mockFetch, type MockRoute } from '@/test/fetch-mock';
import { libraries } from '@/test/fixtures';
import { authors, books, facets, narrators } from '@/test/library-fixtures';
import { renderApp } from '@/test/render-app';
import { signedInRoutes, withPodcastLibrary } from '@/test/routes';

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
    const { router } = renderApp('/library/authors');
    const grid = await screen.findByRole('list', { name: 'Authors' });
    const tiles = within(grid).getAllByRole('link');
    expect(tiles[0]).toHaveTextContent('Brandon Sanderson');
    expect(tiles[0]).toHaveTextContent('2 books · 91h');
    expect(tiles).toHaveLength(4);
    expect(screen.getByText('4 authors, most books first')).toBeInTheDocument();

    const user = userEvent.setup();
    const box = screen.getByRole('textbox', { name: 'Filter authors' });
    await user.type(box, 'wells');
    // The search applies once typing pauses, and lives in the URL.
    await waitFor(() => expect(router.state.location.search).toEqual({ q: 'wells' }));
    await waitFor(() => expect(within(grid).getAllByRole('link')).toHaveLength(1));
    expect(within(grid).getByRole('link')).toHaveTextContent('Martha Wells');
    await user.clear(box);
    await user.type(box, 'austen');
    expect(await screen.findByText('Nothing matches “austen”.')).toBeInTheDocument();
  });

  it('pages the tiles, keeping the page and search in the URL', async () => {
    const many = Array.from({ length: 250 }, (_, i) => ({
      name: `Author ${String(i + 1).padStart(3, '0')}`,
      books: 1,
      duration: 3600,
    }));
    mockFetch(routes({ 'GET /admin/authors': { body: { ...authors, authors: many } } }));
    const user = userEvent.setup();
    const { router } = renderApp('/library/authors?q=author');
    const grid = await screen.findByRole('list', { name: 'Authors' });
    expect(within(grid).getAllByRole('link')).toHaveLength(120);
    expect(screen.getByRole('textbox', { name: 'Filter authors' })).toHaveValue('author');
    const pager = screen.getByRole('navigation', { name: 'Pages' });
    await user.click(within(pager).getByRole('button', { name: 'Page 3' }));
    await waitFor(() => expect(router.state.location.search).toEqual({ q: 'author', page: 3 }));
    expect(within(grid).getAllByRole('link')).toHaveLength(10);
    expect(within(grid).getAllByRole('link')[0]).toHaveTextContent('Author 241');
    // A new search starts again on the first page.
    await user.type(screen.getByRole('textbox', { name: 'Filter authors' }), ' 24');
    await waitFor(() => expect(router.state.location.search).toEqual({ q: 'author 24' }));
    expect(screen.queryByRole('navigation', { name: 'Pages' })).toBeNull();
    // A search replaces its history entry, as on Series; Back goes to the entry
    // before the page was picked.
    router.history.back();
    await waitFor(() => expect(router.state.location.search).toEqual({ q: 'author' }));
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

  it('leaves a podcast library out of the filter, and reads its id in the link as all', async () => {
    const calls = mockFetch(routes(withPodcastLibrary()));
    renderApp('/library/authors?library=3');
    await screen.findByRole('list', { name: 'Authors' });
    const filter = screen.getByRole('group', { name: 'Library' });
    expect(within(filter).getByRole('button', { name: 'Kids' })).toBeInTheDocument();
    expect(within(filter).queryByRole('button', { name: 'Podcasts' })).toBeNull();
    await waitFor(() =>
      expect(within(filter).getByRole('button', { name: 'All' })).toHaveAttribute(
        'aria-pressed',
        'true',
      ),
    );
    await waitFor(() =>
      expect(
        calls
          .filter((c) => c.path === '/admin/authors')
          .at(-1)!
          .query.get('library_id'),
      ).toBeNull(),
    );
  });

  it('merges spellings, and undo puts each book back', async () => {
    const fresh = { library_id: 1, path: 'Sanderson/Elantris' };
    const edited = { library_id: 1, path: 'Sanderson/Warbreaker' };
    let finish!: () => void;
    const pending = new Promise<void>((resolve) => {
      finish = resolve;
    });
    const calls = mockFetch(
      routes({
        'POST /admin/books/merge': async () => {
          await pending;
          return {
            body: {
              books: [
                { ...fresh, previous: null },
                { ...edited, previous: 'Sanderson, Brandon', source: 'community' },
              ],
            },
          };
        },
        'POST /admin/books/bulk': { body: { updated: 1 } },
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
    expect(screen.getByRole('button', { name: 'Merging...' })).toBeDisabled();
    finish();
    expect(await screen.findByText('Merged into Brandon Sanderson')).toBeInTheDocument();
    expect(
      screen.getByText('2 books updated and locked. Revert from each book page.'),
    ).toBeInTheDocument();
    expect(calls.filter((c) => c.path === '/admin/books/merge').map((c) => c.body)).toEqual([
      { field: 'author', sources: ['Sanderson, Brandon'], target: 'Brandon Sanderson' },
    ]);
    expect(
      calls.filter((c) => c.path === '/admin/books' || c.path === '/admin/books/bulk'),
    ).toEqual([]);

    await user.click(screen.getByRole('button', { name: 'Undo' }));
    expect(await screen.findByText('Merge undone')).toBeInTheDocument();
    expect(calls.filter((c) => c.path === '/admin/books/bulk').map((c) => c.body)).toEqual([
      { books: [fresh], revert: ['author'] },
      { books: [edited], set: { author: 'Sanderson, Brandon' }, source: 'community' },
    ]);
  });

  it('keeps undoing past a failed step and offers Undo again for its books', async () => {
    const fresh = { library_id: 1, path: 'Sanderson/Elantris' };
    const edited = { library_id: 1, path: 'Sanderson/Warbreaker' };
    let bulkCalls = 0;
    const calls = mockFetch(
      routes({
        'POST /admin/books/merge': {
          body: {
            books: [
              { ...fresh, previous: null },
              { ...edited, previous: 'Sanderson, Brandon', source: 'edited' },
            ],
          },
        },
        'POST /admin/books/bulk': () =>
          ++bulkCalls === 1
            ? { status: 500, body: { error: 'could not save the edit' } }
            : { body: { updated: 1 } },
      }),
    );
    renderApp('/library/authors');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Merge authors' }));
    await screen.findByText('Merged into Brandon Sanderson');
    await user.click(screen.getByRole('button', { name: 'Undo' }));

    const failed = (await screen.findByText("Couldn't undo the merge")).closest(
      '[data-slot="toast"]',
    ) as HTMLElement;
    const bulk = () => calls.filter((c) => c.path === '/admin/books/bulk').map((c) => c.body);
    // The failed first step didn't stop the second.
    expect(bulk()).toEqual([
      { books: [fresh], revert: ['author'] },
      { books: [edited], set: { author: 'Sanderson, Brandon' }, source: 'edited' },
    ]);
    await user.click(within(failed).getByRole('button', { name: 'Undo' }));
    expect(await screen.findByText('Merge undone')).toBeInTheDocument();
    expect(screen.getByText('1 book has its old spelling again.')).toBeInTheDocument();
    expect(bulk().slice(2)).toEqual([{ books: [fresh], revert: ['author'] }]);
  });

  it.each([
    [
      { status: 500, body: { error: 'could not merge the spellings' } },
      "Couldn't merge into Brandon Sanderson",
    ],
    [{ body: { books: [] } }, 'Nothing to merge: no book carries the other spelling any more'],
  ] as const)('offers no Undo when a merge fails or changes no books', async (response, title) => {
    mockFetch(routes({ 'POST /admin/books/merge': response }));
    renderApp('/library/authors');
    await userEvent.setup().click(await screen.findByRole('button', { name: 'Merge authors' }));
    expect(await screen.findByText(title)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Merge authors' })).toBeEnabled();
    expect(screen.queryByRole('button', { name: 'Undo' })).toBeNull();
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
                other_books: 1,
              },
            ],
          },
        },
        'POST /admin/books/merge': {
          body: { books: [{ library_id: 1, path: 'x', previous: null }] },
        },
      }),
    );
    renderApp('/library/narrators');
    await userEvent.setup().click(await screen.findByRole('button', { name: 'Merge narrators' }));
    expect(
      await screen.findByText('Merged into Michael Kramer & Kate Reading'),
    ).toBeInTheDocument();
    expect(calls.find((c) => c.path === '/admin/books/merge')?.body).toEqual({
      field: 'narrator',
      sources: ['Kramer, Michael & Reading, Kate'],
      target: 'Michael Kramer & Kate Reading',
    });
  });
});
