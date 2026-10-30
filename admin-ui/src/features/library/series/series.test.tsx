import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { setToken } from '@/api/token';
import type { AdminBook, BookMeta, BookRef, SeriesCount } from '@/api/types';
import { mockFetch, type MockReply, type MockRequest, type MockRoute } from '@/test/fetch-mock';
import { serverInfo } from '@/test/fixtures';
import { adminBook, bookDetail, books, series } from '@/test/library-fixtures';
import { seriesKey } from '@/lib/series-key';
import { renderApp } from '@/test/render-app';
import { formatReleaseDate } from './series-model';
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

/**
 * GET /admin/books as the server answers a card: the books in the series= asked
 * for, any spelling (seriesKey), from `all`.
 */
function seriesBooks(all: AdminBook[]): MockRoute {
  return (req) => {
    const key = seriesKey(req.query.get('series') ?? '');
    return {
      body: { books: all.filter((b) => b.series_list.some((s) => seriesKey(s.name) === key)) },
    };
  };
}

/** n one-book series named "Saga 1".."Saga n" by Someone, each changed by `over`. */
function sagas(n: number, over: (i: number) => Partial<SeriesCount> = () => ({})): SeriesCount[] {
  return Array.from({ length: n }, (_, i) => ({
    name: `Saga ${i + 1}`,
    author: 'Someone',
    books: 1,
    duration: 1,
    positions: [1],
    extra_books: 0,
    ...over(i),
  }));
}

function routes(over: Record<string, MockRoute> = {}) {
  return signedInRoutes({
    'GET /admin/series': { body: { series } },
    'GET /admin/books': seriesBooks(books),
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
    // Each card asks for its own series' books, in their order in it.
    const asked = calls.filter((c) => c.path === '/admin/books');
    expect(asked.map((c) => [c.query.get('series'), c.query.get('sort')]).sort()).toEqual([
      ['The Murderbot Diaries', 'series'],
      ['The Stormlight Archive', 'series'],
    ]);

    // All Systems Red has no ASIN or ISBN: no rail to compare with.
    const murderbot = screen.getByRole('region', { name: 'The Murderbot Diaries' });
    expect(murderbot).toHaveTextContent('Martha Wells · 1 book');
    expect(
      within(murderbot).getByText("Match a book of this series to see what's missing"),
    ).toBeInTheDocument();
  });

  it('dates a missing entry, and says which are preorders and new releases', async () => {
    const recent = new Date(Date.now() - 10 * 86_400_000).toISOString().slice(0, 10);
    const dated: BookMeta = {
      ...wayOfKingsMeta,
      series: wayOfKingsMeta.series!.map((r) =>
        r.id !== 'stormlight'
          ? r
          : {
              ...r,
              works: r.works.map((w) =>
                w.title === 'Oathbringer'
                  ? { ...w, release_date: '2017' }
                  : w.title === 'Rhythm of War'
                    ? { ...w, release_date: recent }
                    : w.title === 'Wind and Truth'
                      ? { ...w, release_date: '2099-03' }
                      : w,
              ),
            },
      ),
    };
    mockFetch(routes({ 'GET /libraries/1/meta': { body: dated } }));
    renderApp('/library/series');
    const card = await screen.findByRole('region', { name: 'The Stormlight Archive' });
    const old = await within(card).findByRole('img', {
      name: '#3 Oathbringer (2017): not in your library',
    });
    expect(old).toHaveTextContent('2017');
    const date = formatReleaseDate(recent, 'en');
    const fresh = within(card).getByRole('img', {
      name: `#4 Rhythm of War: new (${date}), not in your library`,
    });
    expect(fresh.querySelector('.spine-date')).toHaveTextContent(`New · ${date}`);
    expect(fresh.querySelector('.spine-date')).toHaveAttribute('data-state', 'new');
    const preorder = within(card).getByRole('img', {
      name: '#5 Wind and Truth: preorder, out Mar 2099',
    });
    expect(preorder.querySelector('.spine-date')).toHaveTextContent('Preorder · Mar 2099');
    // A plain date carries no state word.
    expect(old.querySelector('.spine-date')?.textContent).toBe('2017');
  });

  it("shows no gaps when no community rail is the series (never another series')", async () => {
    // Only the wider Cosmere rail came back: it isn't named like the series, and
    // none of the series' books are on it.
    const calls = mockFetch(
      routes({
        'GET /libraries/1/meta': {
          body: { ...wayOfKingsMeta, series: [wayOfKingsMeta.series![0]] },
        },
      }),
    );
    renderApp('/library/series');
    const card = await screen.findByRole('region', { name: 'The Stormlight Archive' });
    await waitFor(() => expect(calls.some((c) => c.path === '/admin/books/works')).toBe(true));
    await waitFor(() => expect(within(card).getByRole('button', { name: '#2 Words of Radiance' })));
    expect(within(card).queryByRole('img')).toBeNull();
    expect(card).not.toHaveTextContent(/missing/);
    expect(within(card).queryByText(/\/\d/)).toBeNull();
  });

  it("places a book on its community entry when its series index doesn't say", async () => {
    // Words of Radiance carries the series but no number; its ASIN says it is book 2.
    const unnumbered = [books[0], { ...books[1], series_index: 0 }, ...books.slice(2)];
    let release = () => {};
    const calls = mockFetch(
      routes({
        'GET /admin/books': seriesBooks(unnumbered),
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
    mockFetch(routes({ 'GET /admin/books': seriesBooks(coloured) }));
    renderApp('/library/series');
    const card = await screen.findByRole('region', { name: 'The Stormlight Archive' });
    const first = await within(card).findByRole('button', { name: '#1 The Way of Kings' });
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
      .click(await within(card).findByRole('button', { name: '#2 Words of Radiance' }));
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
        'GET /admin/books': seriesBooks([...books, elantris]),
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

  it('is one card for every spelling of a series, all its books placed', async () => {
    // Thirteen books under three spellings; the matched ones (1, 6, 9, 11, 12)
    // say "with", as the community does. The server groups them under that name
    // and the card's series= filter brings back all thirteen.
    const spellings = ['He Who Fights With Monsters', 'He Who Fights WIth Monsters'];
    const matched = [1, 6, 9, 11, 12];
    const monsters = Array.from({ length: 13 }, (_, i) => {
      const n = i + 1;
      return adminBook({
        path: `Shirtaloon/FWM${n}`,
        title: `He Who Fights with Monsters ${n}`,
        author: 'Shirtaloon',
        series: matched.includes(n) ? 'He Who Fights with Monsters' : spellings[n % 2],
        series_index: n,
        asin: matched.includes(n) ? `B0${n}` : '',
      });
    });
    const rail = Array.from({ length: 13 }, (_, i) =>
      work(String(i + 1), `He Who Fights with Monsters ${i + 1}`),
    );
    mockFetch(
      routes({
        'GET /admin/series': {
          body: {
            series: [
              {
                name: 'He Who Fights with Monsters',
                author: 'Shirtaloon',
                books: 13,
                duration: 13,
                positions: rail.map((_, i) => i + 1),
                extra_books: 0,
              },
            ],
          },
        },
        'GET /admin/books': seriesBooks(monsters),
        'GET /libraries/1/meta': {
          body: {
            matched: true,
            series: [
              { id: 'hwfwm', name: 'He Who Fights with Monsters', position: '1', works: rail },
            ],
          },
        },
        'POST /admin/books/works': (req) => ({
          body: {
            works: (req.body as { books: BookRef[] }).books.map((r) => ({
              ...r,
              work_id: monsters.find((b) => b.path === r.path)?.title ?? '',
              failed: false,
            })),
          },
        }),
      }),
    );
    renderApp('/library/series');
    const card = await screen.findByRole('region', { name: 'He Who Fights with Monsters' });
    expect(screen.getAllByRole('region', { name: /monsters/i })).toHaveLength(1);
    await waitFor(() => expect(card).toHaveTextContent('Complete: you have all 13'));
    expect(within(card).queryByRole('img', { name: /not in your library/ })).toBeNull();
    expect(within(card).getByRole('button', { name: /^#4 / })).toBeInTheDocument();
  });

  it('pages the cards, and a page asks only for its own series', async () => {
    // Two-digit names, so the list sorts in number order.
    const many = sagas(60, (i) => ({ name: `Saga ${String(i + 1).padStart(2, '0')}` }));
    const calls = mockFetch(
      routes({
        'GET /admin/series': { body: { series: many } },
        'GET /admin/books': (req) => ({
          body: {
            books: [
              adminBook({
                path: `${req.query.get('series')}/1`,
                title: `${req.query.get('series')} One`,
                series: req.query.get('series') ?? '',
                asin: '',
              }),
            ],
          },
        }),
      }),
    );
    const user = userEvent.setup();
    const { router } = renderApp('/library/series');
    expect(await screen.findByRole('region', { name: 'Saga 01' })).toBeInTheDocument();
    expect(screen.getAllByRole('region', { name: /^Saga/ })).toHaveLength(24);
    const pager = screen.getByRole('navigation', { name: 'Pages' });
    expect(within(pager).getByRole('button', { name: 'Page 1' })).toHaveAttribute(
      'aria-current',
      'page',
    );
    expect(within(pager).getByRole('button', { name: 'Previous' })).toBeDisabled();

    await user.click(within(pager).getByRole('button', { name: 'Page 3' }));
    await waitFor(() => expect(router.state.location.search).toEqual({ page: 3 }));
    expect(await screen.findByRole('region', { name: 'Saga 49' })).toBeInTheDocument();
    expect(screen.getAllByRole('region', { name: /^Saga/ })).toHaveLength(12);
    // Only this page's series were asked for: no walk through the first two pages.
    await waitFor(() => expect(calls.some((c) => c.query.get('series') === 'Saga 60')).toBe(true));
    expect(calls.some((c) => c.query.get('series') === 'Saga 30')).toBe(false);

    // Back returns to the page before.
    router.history.back();
    expect(await screen.findByRole('region', { name: 'Saga 01' })).toBeInTheDocument();
  });

  it('finds a series by name or author, every spelling, from page 1', async () => {
    const many = sagas(30);
    mockFetch(
      routes({
        'GET /admin/series': {
          body: {
            series: [
              ...many,
              { ...many[0], name: 'He Who Fights with Monsters', author: 'Shirtaloon' },
              { ...many[0], name: 'Earthsea', author: 'Ursula K. Le Guin' },
            ],
          },
        },
      }),
    );
    const user = userEvent.setup();
    const { router } = renderApp('/library/series?page=2');
    expect(await screen.findByRole('region', { name: 'Earthsea' })).toBeInTheDocument();

    const box = screen.getByRole('textbox', { name: 'Find a series or author' });
    await user.type(box, 'fights  WITH');
    // The search applies once typing pauses.
    await waitFor(() => expect(router.state.location.search).toEqual({ q: 'fights  WITH' }));
    expect(await screen.findByRole('region', { name: 'He Who Fights with Monsters' }));
    expect(screen.queryAllByRole('region', { name: /^(Saga|Earthsea)/ })).toHaveLength(0);

    expect(screen.queryByRole('navigation', { name: 'Pages' })).toBeNull();

    await user.clear(box);
    await user.type(box, 'le guin');
    expect(await screen.findByRole('region', { name: 'Earthsea' })).toBeInTheDocument();

    await user.clear(box);
    await user.type(box, 'nothing like it');
    expect(await screen.findByText('Nothing matches “nothing like it”.')).toBeInTheDocument();
  });

  it('comes back to the same page and search from a book', async () => {
    const many = sagas(30);
    mockFetch(routes({ 'GET /admin/series': { body: { series: [...many, ...series] } } }));
    const user = userEvent.setup();
    const { router } = renderApp('/library/series?q=a&page=2');
    const card = await screen.findByRole('region', { name: 'The Stormlight Archive' });
    await user.click(await within(card).findByRole('button', { name: '#2 Words of Radiance' }));
    await waitFor(() => expect(router.state.location.pathname).toBe('/library/book'));
    router.history.back();
    expect(
      await screen.findByRole('region', { name: 'The Stormlight Archive' }),
    ).toBeInTheDocument();
    expect(router.state.location.search).toEqual({ q: 'a', page: 2 });
    expect(screen.getByRole('textbox', { name: 'Find a series or author' })).toHaveValue('a');
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
