import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { setToken } from '@/api/token';
import { mockFetch, type MockRoute } from '@/test/fetch-mock';
import { kidsShare, users } from '@/test/fixtures';
import { authors, books, narrators, series } from '@/test/library-fixtures';
import { renderApp } from '@/test/render-app';
import { signedInRoutes } from '@/test/routes';

// The palette's content search: books from the server's full text, people,
// authors, series, narrators and shares matched in the browser.

function routes(over: Record<string, MockRoute> = {}) {
  return signedInRoutes({
    'GET /admin/books': (req) => ({
      body: {
        books: books.filter((b) =>
          `${b.title} ${b.author}`.toLowerCase().includes((req.query.get('q') ?? '').toLowerCase()),
        ),
      },
    }),
    'GET /admin/authors': { body: authors },
    'GET /admin/narrators': { body: narrators },
    'GET /admin/series': { body: { series } },
    'GET /admin/users': { body: { users } },
    'GET /admin/shares': { body: { shares: [kidsShare] } },
    ...over,
  });
}

async function openPalette() {
  await screen.findByRole('heading', { level: 1, name: /chris\.$/ });
  fireEvent.keyDown(window, { key: 'k', ctrlKey: true });
  return screen.findByRole('combobox');
}

beforeEach(() => setToken('stored'));

describe('palette search', () => {
  it('finds a book by full text and opens its page', async () => {
    const calls = mockFetch(routes());
    const { router } = renderApp();
    const user = userEvent.setup();
    await user.type(await openPalette(), 'way of');
    const option = await screen.findByRole('option', { name: /The Way of Kings/ });
    // A content hit is a result: no "nothing matches" next to it, and it is counted.
    expect(screen.queryByText(/Nothing matches/)).not.toBeInTheDocument();
    expect(screen.getByText(/^[1-9]\d* results?$/)).toBeInTheDocument();
    expect(calls.find((c) => c.path === '/admin/books')?.query.get('q')).toBe('way of');
    await user.click(option);
    await waitFor(() => expect(router.state.location.pathname).toBe('/library/book'));
    expect(router.state.location.search).toEqual({
      library: 1,
      path: 'Brandon Sanderson/The Stormlight Archive/01 - The Way of Kings',
    });
  });

  it('offers authors, series and narrators that open the filtered book list', async () => {
    mockFetch(routes());
    const { router } = renderApp();
    const user = userEvent.setup();
    await user.type(await openPalette(), 'sanderson');
    const group = await screen.findByRole('group', { name: 'Authors' });
    // Both spellings, the one starting with the search first.
    const options = within(group).getAllByRole('option');
    expect(options.map((o) => o.textContent)).toEqual([
      expect.stringContaining('Sanderson, Brandon'),
      expect.stringContaining('Brandon Sanderson'),
    ]);
    await user.click(options[1]);
    await waitFor(() =>
      expect(router.state.location.search).toEqual({ author: 'Brandon Sanderson' }),
    );
    expect(router.state.location.pathname).toBe('/library');
  });

  it('finds people and shares', async () => {
    mockFetch(routes());
    const { router } = renderApp();
    const user = userEvent.setup();
    const input = await openPalette();
    await user.type(input, 'cosy');
    expect(
      await within(await screen.findByRole('group', { name: 'Shares' })).findByRole('option', {
        name: /Cosy mysteries/,
      }),
    ).toBeInTheDocument();
    await user.clear(input);
    await user.type(input, 'sam');
    await user.click(
      await within(await screen.findByRole('group', { name: 'People' })).findByRole('option', {
        name: /sam/,
      }),
    );
    await waitFor(() => expect(router.state.location.pathname).toBe('/people/user/2'));
  });

  it('searches nothing until two characters are typed, and offers the full book search', async () => {
    const calls = mockFetch(routes());
    const { router } = renderApp();
    const user = userEvent.setup();
    const input = await openPalette();
    await user.type(input, 'w');
    expect(calls.some((c) => c.path.startsWith('/admin/books'))).toBe(false);
    expect(calls.some((c) => c.path === '/admin/authors')).toBe(false);
    await user.type(input, 'xyz');
    // The highlighted match is its own element, so the accessible name spaces it out.
    await user.click(
      await screen.findByRole('option', { name: /Search all books for “ ?wxyz ?”/ }),
    );
    await waitFor(() => expect(router.state.location.search).toEqual({ q: 'wxyz' }));
  });

  // The server's books arrive after the static entries matched (ADMIN-13): the
  // highlight Enter acts on follows them to the top, unless the user moved it.
  function slowBooks() {
    let release = () => {};
    const answered = new Promise<void>((r) => (release = r));
    const route: MockRoute = async () => {
      await answered;
      return { body: { books: [books[0]] } };
    };
    return { route, release };
  }
  const selectedOption = () =>
    screen.getAllByRole('option').find((o) => o.getAttribute('aria-selected') === 'true');

  it('moves the highlight to the first result when results arrive above it', async () => {
    const slow = slowBooks();
    mockFetch(routes({ 'GET /admin/books': slow.route }));
    const { router } = renderApp();
    const user = userEvent.setup();
    await user.type(await openPalette(), 'dark');
    await screen.findByRole('option', { name: /Search all books/ });
    expect(selectedOption()).toHaveAccessibleName(/Use the dark theme/);
    slow.release();
    await screen.findByRole('option', { name: /The Way of Kings/ });
    await waitFor(() => expect(selectedOption()).toHaveAccessibleName(/The Way of Kings/));
    await user.keyboard('{Enter}');
    await waitFor(() => expect(router.state.location.pathname).toBe('/library/book'));
  });

  it('leaves the highlight where the user moved it when results arrive', async () => {
    const slow = slowBooks();
    mockFetch(routes({ 'GET /admin/books': slow.route }));
    renderApp();
    const user = userEvent.setup();
    await user.type(await openPalette(), 'dark');
    await screen.findByRole('option', { name: /Search all books/ });
    await user.keyboard('{ArrowDown}');
    expect(selectedOption()).toHaveAccessibleName(/Search all books/);
    slow.release();
    await screen.findByRole('option', { name: /The Way of Kings/ });
    expect(selectedOption()).toHaveAccessibleName(/Search all books/);
  });
});
