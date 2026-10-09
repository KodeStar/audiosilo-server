import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { setToken } from '@/api/token';
import type { Activity } from '@/api/types';
import { mockFetch, type MockRequest, type MockRoute } from '@/test/fetch-mock';
import { activity, admin, liveSession, sam, stats } from '@/test/fixtures';
import { renderApp } from '@/test/render-app';
import { signedInRoutes } from '@/test/routes';

// The Activity destination: Overview, Live now, Sessions and Year in listening.

/** GET /admin/stats answering ?range= with an activity period (`by` picks it per range). */
function statsRoute(by: (range: string) => Activity = (range) => activity({ range })): MockRoute {
  return (req: MockRequest) => {
    const range = req.query.get('range');
    // Like the server, "year" answers for (and is labelled with) the current year.
    const label = range === 'year' ? String(new Date().getFullYear()) : range;
    return { body: label ? { activity: by(label) } : stats() };
  };
}

function routes(over: Record<string, MockRoute> = {}) {
  return signedInRoutes({
    'GET /admin/stats': statsRoute(),
    // The year calendar's days-only query.
    'GET /admin/listening': (req) => ({
      body: { ...activity({ range: req.query.get('range')! }), range: req.query.get('range') },
    }),
    'GET /admin/users': { body: { users: [admin, sam] } },
    ...over,
  });
}

// The screens load as lazy chunks (the charts one is big): load them once up front so
// the first test doesn't spend its waits on the import.
beforeAll(async () => {
  await Promise.all([import('./activity-page'), import('./year-page'), import('./live-page')]);
});
beforeEach(() => setToken('stored'));
afterEach(() => vi.unstubAllGlobals());

describe('activity overview', () => {
  it('shows the period: tiles with changes, charts, top books and people, playback and apps', async () => {
    const calls = mockFetch(routes());
    renderApp('/activity');
    expect(await screen.findByRole('heading', { level: 1, name: 'Activity' })).toBeInTheDocument();
    expect(await screen.findByText('9h')).toBeInTheDocument();
    expect(screen.getByText('2 people listened')).toBeInTheDocument();
    // 9h against 6h before: up 50%.
    expect(screen.getByText('50%')).toBeInTheDocument();
    expect(screen.getByText('25% finish rate · 1 of 4 started')).toBeInTheDocument();
    expect(
      screen.getByRole('img', { name: 'Listening hours over the period, by person' }),
    ).toBeInTheDocument();
    expect(screen.getByText('Busiest: Saturday 21:00')).toBeInTheDocument();

    const books = screen.getByRole('region', { name: 'Top books' });
    expect(within(books).getByRole('link', { name: /Project Hail Mary/ })).toHaveAttribute(
      'href',
      expect.stringContaining('/admin/library/book?library=1'),
    );
    const people = screen.getByRole('region', { name: 'Top people' });
    expect(within(people).getByRole('link', { name: /sam/ })).toHaveAttribute(
      'href',
      '/admin/people/user/2',
    );
    // The drop-off names its chapter and links to the read problem.
    expect(screen.getByText(/3 people stopped at Part 7/)).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'See the read problem' })).toHaveAttribute(
      'href',
      '/admin/health?issue=scan_error',
    );
    const playback = screen.getByRole('region', { name: 'How it played' });
    expect(within(playback).getByText('Transcoded OPUS')).toBeInTheDocument();
    expect(within(playback).getByText('89%')).toBeInTheDocument();
    const apps = screen.getByRole('region', { name: 'Apps in use' });
    expect(within(apps).getByText('AudioSilo 1.3.0 · iOS')).toBeInTheDocument();
    expect(
      within(apps).getByText('1 device runs an older build than others on its platform.'),
    ).toBeInTheDocument();
    expect(screen.getByText("1 person hasn't been active for 60 days")).toBeInTheDocument();
    // The default period, and only the year's days for the calendar.
    const ranges = calls.filter((c) => c.path === '/admin/stats').map((c) => c.query.get('range'));
    expect(ranges).toEqual(['30d']);
    expect(calls.find((c) => c.path === '/admin/listening')?.query.get('range')).toBe('1y');
  });

  it('switches the period and keeps it in the URL', async () => {
    const calls = mockFetch(routes());
    const { router } = renderApp('/activity');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: '7 days' }));
    await waitFor(() => expect(router.state.location.search).toEqual({ range: '7d' }));
    await waitFor(() =>
      expect(calls.some((c) => c.path === '/admin/stats' && c.query.get('range') === '7d')).toBe(
        true,
      ),
    );
    expect(screen.getByRole('button', { name: '7 days' })).toHaveAttribute('aria-pressed', 'true');
  });

  it('says so when nobody listened, and still shows the collection', async () => {
    mockFetch(
      routes({
        'GET /admin/stats': statsRoute((range) =>
          activity({
            range,
            totals: { listened: 0, sessions: 0, listeners: 0, books: 0, finished: 0 },
            inactive_users: [],
          }),
        ),
      }),
    );
    renderApp('/activity');
    expect(await screen.findByText('No listening in this period')).toBeInTheDocument();
    expect(screen.getByRole('region', { name: 'Storage and coverage' })).toBeInTheDocument();
    expect(screen.queryByRole('region', { name: 'Top books' })).not.toBeInTheDocument();
  });

  it('shows a failed load with a retry', async () => {
    mockFetch(
      routes({ 'GET /admin/stats': { status: 500, body: { error: 'could not load activity' } } }),
    );
    renderApp('/activity');
    expect(await screen.findByText("Activity couldn't load")).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Try again' })).toBeInTheDocument();
  });
});

describe('live now', () => {
  it('lists each device with its book, chapter, app, playback and address', async () => {
    mockFetch(
      routes({
        'GET /admin/sessions/live': {
          body: {
            sessions: [
              // A title that names nothing ("024") comes without one: its place names it.
              liveSession({
                id: 1,
                state: 'paused',
                username: 'chris',
                user_id: 1,
                chapter: undefined,
                chapter_index: 23,
              }),
              liveSession({ id: 2, transcoded: true, codec: 'opus' }),
            ],
          },
        },
      }),
    );
    renderApp('/activity/live');
    expect(
      await screen.findByText('1 stream playing · 0 direct, 1 transcoding · 1 paused'),
    ).toBeInTheDocument();
    const items = screen.getAllByRole('article');
    // Playing first.
    expect(within(items[0]).getByText('sam')).toBeInTheDocument();
    expect(within(items[0]).getByText('Chapter 12')).toBeInTheDocument();
    expect(within(items[0]).getByText('AudioSilo 1.4.2 · iOS')).toBeInTheDocument();
    expect(within(items[0]).getByText('Transcode · OPUS')).toBeInTheDocument();
    expect(within(items[0]).getByText('192.168.1.24')).toBeInTheDocument();
    expect(within(items[1]).getByText('Paused')).toBeInTheDocument();
    expect(within(items[1]).getByText('Chapter 24')).toBeInTheDocument();
  });

  it('says when nobody is listening', async () => {
    mockFetch(routes());
    renderApp('/activity/live');
    expect(await screen.findByText('Nobody is listening right now')).toBeInTheDocument();
  });
});

describe('sessions', () => {
  it('lists sessions, filters by person and book, and pages back', async () => {
    const newest = liveSession({ id: 9 });
    const calls = mockFetch(
      routes({
        'GET /admin/sessions': (req) =>
          req.query.get('before')
            ? {
                body: {
                  sessions: [liveSession({ id: 5, title: 'Older book' })],
                  next_before: null,
                },
              }
            : { body: { sessions: [newest], next_before: 9 } },
      }),
    );
    const { router } = renderApp(
      '/activity/sessions?library=1&path=Andy%20Weir%2FProject%20Hail%20Mary',
    );
    expect(await screen.findByText('Book: Project Hail Mary')).toBeInTheDocument();
    expect(await screen.findByRole('link', { name: /Project Hail Mary/ })).toBeInTheDocument();
    const first = calls.find((c) => c.path === '/admin/sessions')!;
    expect(first.query.get('library_id')).toBe('1');
    expect(first.query.get('path')).toBe('Andy Weir/Project Hail Mary');

    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: 'Show older sessions' }));
    expect((await screen.findAllByText('Older book')).length).toBeGreaterThan(0);
    const older = calls.find((c) => c.path === '/admin/sessions' && c.query.get('before') === '9');
    // The last session's start rides along, so a gone cursor session can't restart the list.
    expect(older?.query.get('before_at')).toBe(newest.started_at);

    await user.selectOptions(screen.getByRole('combobox', { name: 'Whose sessions' }), 'sam');
    await waitFor(() => expect(router.state.location.search).toMatchObject({ person: 2 }));
    await user.click(screen.getByRole('button', { name: 'Show every book' }));
    await waitFor(() => expect(router.state.location.search).toEqual({ person: 2 }));
    await waitFor(() =>
      expect(
        calls.some(
          (c) =>
            c.path === '/admin/sessions' &&
            c.query.get('user_id') === '2' &&
            !c.query.has('library_id'),
        ),
      ).toBe(true),
    );
  });

  it('has an empty state', async () => {
    mockFetch(routes({ 'GET /admin/sessions': { body: { sessions: [], next_before: null } } }));
    renderApp('/activity/sessions');
    expect(await screen.findByText('No sessions yet')).toBeInTheDocument();
  });
});

describe('year in listening', () => {
  it("tells the year's story and switches years", async () => {
    const year = new Date().getFullYear();
    const calls = mockFetch(routes());
    const { router } = renderApp('/activity/year');
    expect(await screen.findByText('2 people listened for 9h.')).toBeInTheDocument();
    expect(screen.getByText('Book of the year')).toBeInTheDocument();
    expect(screen.getByText(/The most heard voice: Ray Porter/)).toBeInTheDocument();
    // No year picked: the server's current year, which it names.
    expect(calls.some((c) => c.query.get('range') === 'year')).toBe(true);
    const user = userEvent.setup();
    await user.selectOptions(screen.getByRole('combobox', { name: 'Year' }), String(year - 1));
    await waitFor(() => expect(router.state.location.search).toEqual({ year: year - 1 }));
    await waitFor(() =>
      expect(calls.some((c) => c.query.get('range') === String(year - 1))).toBe(true),
    );
  });

  it('has an empty year', async () => {
    mockFetch(
      routes({
        'GET /admin/stats': statsRoute((range) =>
          activity({
            range,
            totals: { listened: 0, sessions: 0, listeners: 0, books: 0, finished: 0 },
          }),
        ),
      }),
    );
    renderApp('/activity/year');
    expect(
      await screen.findByText(`No listening in ${new Date().getFullYear()}`),
    ).toBeInTheDocument();
  });
});

describe('listening from before sessions were recorded', () => {
  it('says how much of the period is estimated, and only then', async () => {
    mockFetch(
      routes({
        'GET /admin/stats': statsRoute((range) => activity({ range, estimated: 5 * 3600 })),
      }),
    );
    renderApp('/activity');
    expect(
      await screen.findByText(
        /Includes about 5\W*h\w* estimated where no session was recorded \(from before this server recorded/,
      ),
    ).toBeInTheDocument();
    // 9 h listened, 5 h of it estimated, 12 sessions: the chart's total and the
    // sessions' average are the 4 h the sessions recorded.
    expect(screen.getByText(/^4\W*h\w* in total$/)).toBeInTheDocument();
    expect(screen.getByText(/^20\W*m\w* on average$/)).toBeInTheDocument();
  });

  it('shows a session made from the app history as such', async () => {
    mockFetch(
      routes({
        'GET /admin/sessions': {
          body: {
            sessions: [liveSession({ id: 3, backfilled: true, device_name: '', client: null })],
            next_before: null,
          },
        },
      }),
    );
    renderApp('/activity/sessions');
    expect(await screen.findByText('Listening history')).toBeInTheDocument();
    expect(screen.getByText('From the app, before sessions were recorded')).toBeInTheDocument();
    expect(screen.queryByText('Unknown app')).not.toBeInTheDocument();
  });

  it('shows a session imported from Audiobookshelf with the device it recorded', async () => {
    mockFetch(
      routes({
        'GET /admin/sessions': {
          body: {
            sessions: [
              liveSession({
                id: 4,
                backfilled: true,
                imported: true,
                device_name: 'Pixel 8',
                client: { app: 'Audiobookshelf', version: '', platform: '' },
              }),
            ],
            next_before: null,
          },
        },
      }),
    );
    renderApp('/activity/sessions');
    expect(await screen.findByText('Imported from Audiobookshelf')).toBeInTheDocument();
    expect(screen.getByText('Pixel 8')).toBeInTheDocument();
    expect(screen.queryByText('Listening history')).not.toBeInTheDocument();
  });
});
