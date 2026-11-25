import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { setToken } from '@/api/token';
import type { ServerEvent } from '@/api/types';
import { mockFetch } from '@/test/fetch-mock';
import { renderApp } from '@/test/render-app';
import { signedInRoutes } from '@/test/routes';

// The top bar's bell: the server's newest events, a dot for what's new since the
// last look, each leading where it can be dealt with.

const events: ServerEvent[] = [
  {
    id: 7,
    at: '2026-10-04T09:00:00.000Z',
    kind: 'library_unavailable',
    data: { library: 'Lectures', library_id: 2 },
  },
  {
    id: 6,
    at: '2026-10-04T08:00:00.000Z',
    kind: 'book_added',
    data: { library: 'Fiction', count: 2, titles: ['Dune', 'Emma'] },
  },
];

beforeEach(() => {
  setToken('stored');
  localStorage.removeItem('audiosilo_events_seen');
});

describe('notifications bell', () => {
  it('marks new events until the bell is opened, and links each', async () => {
    mockFetch(signedInRoutes({ 'GET /admin/events': { body: { events, next_before: 0 } } }));
    const { router } = renderApp('/');
    const user = userEvent.setup();
    const bell = await screen.findByRole('button', { name: 'Notifications, 2 new' });
    await user.click(bell);
    const popup = await screen.findByRole('dialog', { name: 'Notifications' });
    expect(within(popup).getByText('Lectures is offline')).toBeInTheDocument();
    expect(within(popup).getByText('2 new books in Fiction')).toBeInTheDocument();
    expect(within(popup).getByText('Dune, Emma')).toBeInTheDocument();
    expect(localStorage.getItem('audiosilo_events_seen')).toBe('7');
    await user.click(within(popup).getByRole('link', { name: /Lectures is offline/ }));
    expect(router.state.location.pathname).toBe('/library/libraries');
    expect(await screen.findByRole('button', { name: 'Notifications' })).toBeInTheDocument();
  });

  it('leads to the whole feed', async () => {
    mockFetch(signedInRoutes({ 'GET /admin/events': { body: { events, next_before: 0 } } }));
    const { router } = renderApp('/');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Notifications, 2 new' }));
    const popup = await screen.findByRole('dialog', { name: 'Notifications' });
    await user.click(within(popup).getByRole('link', { name: 'See all' }));
    expect(router.state.location.pathname).toBe('/server/events');
    expect(await screen.findByRole('heading', { name: 'Events', level: 1 })).toBeInTheDocument();
  });

  it('reads the feed again on opening, so what just happened shows (server#383)', async () => {
    const failed: ServerEvent = {
      id: 8,
      at: '2026-10-04T10:00:00.000Z',
      kind: 'backup_failed',
      data: { trigger: 'manual', error: 'permission_denied' },
    };
    let reads = 0;
    mockFetch(
      signedInRoutes({
        'GET /admin/events': () => ({
          body: { events: reads++ === 0 ? events : [failed, ...events], next_before: 0 },
        }),
      }),
    );
    renderApp('/');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Notifications, 2 new' }));
    const popup = await screen.findByRole('dialog', { name: 'Notifications' });
    expect(await within(popup).findByText('A backup failed')).toBeInTheDocument();
    const reason = within(popup).getByText(/Check that the mount allows writes/);
    expect(reason).toHaveTextContent(/read and write access/);
    expect(reason).not.toHaveTextContent(/PUID|PGID/);
    // Seen while open: no dot left behind.
    expect(localStorage.getItem('audiosilo_events_seen')).toBe('8');
  });

  it('says when nothing has happened', async () => {
    mockFetch(signedInRoutes());
    renderApp('/');
    await userEvent.setup().click(await screen.findByRole('button', { name: 'Notifications' }));
    expect(await screen.findByText(/Nothing yet/)).toBeInTheDocument();
  });
});
