import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { setToken } from '@/api/token';
import type { BookMeta, BookRef } from '@/api/types';
import { mockFetch, type MockReply, type MockRequest, type MockRoute } from '@/test/fetch-mock';
import { serverInfo } from '@/test/fixtures';
import { adminBook, bookDetail, books, series } from '@/test/library-fixtures';
import { renderApp } from '@/test/render-app';
import { signedInRoutes } from '@/test/routes';

const work = (position: string, title: string) => ({
  id: title,
  title,
  position,
  authors: [],
  web_url: '',
});

// The Way of Kings' community metadata: a five-book rail, plus a wider family
// rail that must not be picked over the one named like the series.
const wayOfKingsMeta: BookMeta = {
  matched: true,
  series: [
    { id: 'cosmere', name: 'Cosmere', position: '9', works: [work('1', 'Elantris')] },
    {
      id: 'stormlight',
      name: 'The Stormlight Archive',
      position: '1',
      works: [
        work('1', 'The Way of Kings'),
        work('2', 'Words of Radiance'),
        work('3', 'Oathbringer'),
        work('4', 'Rhythm of War'),
        work('5', 'Wind and Truth'),
      ],
    },
  ],
};

/** POST /admin/books/works: each fixture book is the work named like its title. */
function worksReply(req: MockRequest): MockReply {
  const refs = (req.body as { books: BookRef[] }).books;
  return {
    body: {
      works: refs.map((r) => ({
        ...r,
        work_id: books.find((b) => b.path === r.path)?.title ?? '',
        failed: false,
      })),
    },
  };
}

function routes(over: Record<string, MockRoute> = {}) {
  return signedInRoutes({
    'GET /admin/series': { body: { series } },
    'GET /admin/books': { body: { books } },
    'GET /libraries/1/meta': { body: wayOfKingsMeta },
    'POST /admin/books/works': worksReply,
    // The book page a spine opens.
    'GET /admin/libraries/1/book': { body: bookDetail() },
    ...over,
  });
}

beforeEach(() => setToken('stored'));

describe('series', () => {
  it('shows owned books as spines and missing entries as gaps', async () => {
    const calls = mockFetch(routes());
    renderApp('/library/series');
    expect(
      await screen.findByText(/Series order and missing entries come from community metadata/),
    ).toBeInTheDocument();

    const card = await screen.findByRole('region', { name: 'The Stormlight Archive' });
    expect(within(card).getByRole('button', { name: '#1 The Way of Kings' })).toBeInTheDocument();
    expect(within(card).getByRole('button', { name: '#2 Words of Radiance' })).toBeInTheDocument();
    expect(
      await within(card).findByRole('img', { name: '#3 Oathbringer: not in your library' }),
    ).toHaveAttribute('title', '#3 Oathbringer: not in your library');
    expect(within(card).getByRole('img', { name: '#5 Wind and Truth: not in your library' }));
    expect(card).toHaveTextContent('Brandon Sanderson · You have 1, 2 of 5; missing 3-5');
    expect(within(card).getByText('2/5')).toHaveAccessibleName('2 of 5 on your server');

    // The rail came from the first book with an identifier.
    const meta = calls.filter((c) => c.path === '/libraries/1/meta');
    expect(meta.map((c) => c.query.get('path'))).toEqual([books[0].path]);
    expect(calls.find((c) => c.path === '/admin/books')?.query.get('sort')).toBe('series');

    // All Systems Red has no ASIN or ISBN: no rail to compare with.
    const murderbot = screen.getByRole('region', { name: 'The Murderbot Diaries' });
    expect(murderbot).toHaveTextContent('Martha Wells · 1 book');
    expect(
      within(murderbot).getByText("Match a book of this series to see what's missing"),
    ).toBeInTheDocument();
  });

  it("places a book on its community entry when its series index doesn't say", async () => {
    // Words of Radiance carries the series but no number; its ASIN says it is book 2.
    const unnumbered = [books[0], { ...books[1], series_index: 0 }, ...books.slice(2)];
    let release = () => {};
    const calls = mockFetch(
      routes({
        'GET /admin/books': { body: { books: unnumbered } },
        'POST /admin/books/works': (req) =>
          new Promise<void>((resolve) => (release = resolve)).then(() => worksReply(req)),
      }),
    );
    renderApp('/library/series');
    const card = await screen.findByRole('region', { name: 'The Stormlight Archive' });
    // Every matched book is asked about, the rail's own too, in one request.
    await waitFor(() => expect(calls.some((c) => c.path === '/admin/books/works')).toBe(true));
    const asked = calls.filter((c) => c.path === '/admin/books/works');
    expect(asked.map((c) => (c.body as { books: BookRef[] }).books)).toEqual([
      [
        { library_id: 1, path: books[0].path },
        { library_id: 1, path: books[1].path },
      ],
    ]);
    // While the works load, no gaps (they would be wrong).
    expect(card).toHaveTextContent('Brandon Sanderson · 2 books');
    expect(within(card).queryByRole('img', { name: /not in your library/ })).toBeNull();

    release();
    await waitFor(() => expect(card).toHaveTextContent('You have 1, 2 of 5; missing 3-5'));
    expect(within(card).getByRole('button', { name: '#2 Words of Radiance' })).toBeInTheDocument();
    expect(within(card).queryByRole('img', { name: /#2 Words of Radiance/ })).toBeNull();
  });

  it('falls back to the series index when the works fail', async () => {
    mockFetch(routes({ 'POST /admin/books/works': { status: 502, body: { error: 'down' } } }));
    renderApp('/library/series');
    const card = await screen.findByRole('region', { name: 'The Stormlight Archive' });
    await waitFor(() => expect(card).toHaveTextContent('You have 1, 2 of 5; missing 3-5'));
  });

  it("draws a spine in its cover's colours, as tall as the book is long", async () => {
    const coloured = [
      { ...books[0], duration: 45 * 3600, cover_color: { bg: '#1e2a50', accent: '#f0a020' } },
      { ...books[1], duration: 4 * 3600 },
      ...books.slice(2),
    ];
    mockFetch(routes({ 'GET /admin/books': { body: { books: coloured } } }));
    renderApp('/library/series');
    const card = await screen.findByRole('region', { name: 'The Stormlight Archive' });
    const first = within(card).getByRole('button', { name: '#1 The Way of Kings' });
    expect(first.style.getPropertyValue('--c1')).toBe('#1e2a50');
    const second = within(card).getByRole('button', { name: '#2 Words of Radiance' });
    expect(parseFloat(first.style.height)).toBeGreaterThan(parseFloat(second.style.height));
  });

  it('opens a book from its spine', async () => {
    mockFetch(routes());
    const { router } = renderApp('/library/series');
    const card = await screen.findByRole('region', { name: 'The Stormlight Archive' });
    await userEvent
      .setup()
      .click(within(card).getByRole('button', { name: '#2 Words of Radiance' }));
    await waitFor(() => expect(router.state.location.pathname).toBe('/library/book'));
    expect(router.state.location.search).toEqual({ library: 1, path: books[1].path });
  });

  it('loads its own books for a series some are in beyond their main one', async () => {
    const elantris = adminBook({
      title: 'Elantris',
      path: 'Brandon Sanderson/Elantris',
      series: '',
      series_index: 0,
      series_list: [{ name: 'Cosmere', position: 1 }],
      asin: '',
    });
    const calls = mockFetch(
      routes({
        'GET /admin/series': {
          body: {
            series: [
              ...series,
              {
                name: 'Cosmere',
                author: 'Brandon Sanderson',
                books: 1,
                duration: 1,
                positions: [1],
                extra_books: 1,
              },
            ],
          },
        },
        'GET /admin/books': (req) =>
          req.query.get('series') === 'Cosmere'
            ? { body: { books: [elantris] } }
            : { body: { books } },
      }),
    );
    renderApp('/library/series');
    const card = await screen.findByRole('region', { name: 'Cosmere' });
    expect(await within(card).findByRole('button', { name: '#1 Elantris' })).toBeInTheDocument();
    expect(
      calls.some((c) => c.query.get('series') === 'Cosmere' && c.query.get('sort') === 'series'),
    ).toBe(true);
  });

  it('links the series name to its books', async () => {
    mockFetch(routes());
    const { router } = renderApp('/library/series?library=1');
    const card = await screen.findByRole('region', { name: 'The Stormlight Archive' });
    await userEvent
      .setup()
      .click(within(card).getByRole('link', { name: 'The Stormlight Archive' }));
    await waitFor(() => expect(router.state.location.pathname).toBe('/library'));
    expect(router.state.location.search).toEqual({ library: 1, series: 'The Stormlight Archive' });
  });

  it('says a complete series is complete', async () => {
    mockFetch(
      routes({
        'GET /libraries/1/meta': {
          body: {
            matched: true,
            series: [
              {
                id: 'stormlight',
                name: 'the stormlight archive',
                position: '1',
                works: [work('1', 'The Way of Kings'), work('2', 'Words of Radiance')],
              },
            ],
          },
        },
      }),
    );
    renderApp('/library/series');
    const card = await screen.findByRole('region', { name: 'The Stormlight Archive' });
    await waitFor(() => expect(card).toHaveTextContent('Complete: you have all 2'));
    expect(within(card).queryByRole('img', { name: /not in your library/ })).toBeNull();
  });

  it('pages the series-sorted books only until the series-less ones begin', async () => {
    const calls = mockFetch(
      routes({
        'GET /admin/books': (req) =>
          req.query.get('cursor')
            ? {
                body: {
                  books: [books[1], adminBook({ series: '', path: 'Loose' })],
                  next_cursor: 'p3',
                },
              }
            : { body: { books: [books[0], books[2]], next_cursor: 'p2' } },
      }),
    );
    renderApp('/library/series');
    const card = await screen.findByRole('region', { name: 'The Stormlight Archive' });
    expect(
      await within(card).findByRole('button', { name: '#2 Words of Radiance' }),
    ).toBeInTheDocument();
    await waitFor(() => expect(card).toHaveTextContent('You have 1, 2 of 5'));
    expect(
      calls.filter((c) => c.path === '/admin/books').map((c) => c.query.get('cursor')),
    ).toEqual([null, 'p2']);
  });

  it('without community metadata, says how to see gaps and asks for none', async () => {
    const calls = mockFetch(
      routes({
        'GET /server': {
          body: { ...serverInfo, capabilities: { ...serverInfo.capabilities, metadata: false } },
        },
      }),
    );
    renderApp('/library/series');
    const card = await screen.findByRole('region', { name: 'The Stormlight Archive' });
    expect(
      await screen.findByText(/to see which books of a series you're missing/),
    ).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Turn on community metadata' })).toHaveAttribute(
      'href',
      '/admin/server',
    );
    expect(card).toHaveTextContent('Brandon Sanderson · 2 books');
    expect(within(card).getByRole('button', { name: '#1 The Way of Kings' })).toBeInTheDocument();
    expect(screen.queryByText("Match a book of this series to see what's missing")).toBeNull();
    expect(calls.some((c) => c.path.endsWith('/meta'))).toBe(false);
  });

  it('shows an empty state when no book is in a series', async () => {
    mockFetch(routes({ 'GET /admin/series': { body: { series: [] } } }));
    renderApp('/library/series');
    expect(await screen.findByText('No series yet')).toBeInTheDocument();
    expect(screen.getByText(/None of these books is part of a series/)).toBeInTheDocument();
  });

  it('shows a failure with a retry', async () => {
    mockFetch(routes({ 'GET /admin/series': { status: 500, body: { error: 'database locked' } } }));
    renderApp('/library/series');
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent("Series didn't load");
    expect(within(alert).getByRole('button', { name: 'Try again' })).toBeInTheDocument();
  });
});
