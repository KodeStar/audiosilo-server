import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { setToken } from '@/api/token';
import type { ServerEvent } from '@/api/types';
import { mockFetch } from '@/test/fetch-mock';
import { renderApp } from '@/test/render-app';
import { signedInRoutes } from '@/test/routes';

// Server > Events: the whole feed the bell shows the start of, by kind, a page at a time.

const ev = (id: number, over: Partial<ServerEvent>): ServerEvent => ({
  id,
  at: '2026-10-04T09:00:00.000Z',
  kind: 'book_added',
  data: { library: 'Fiction', count: 1, titles: ['Dune'] },
  ...over,
});

beforeEach(() => setToken('stored'));

describe('events page', () => {
  it('lists the feed, pages back, and narrows to one kind', async () => {
    const calls = mockFetch(
      signedInRoutes({
        'GET /admin/events': (req) => {
          if (req.query.get('kind') === 'new_device') {
            return {
              body: {
                events: [ev(5, { kind: 'new_device', data: { user: 'sam', device: 'iPad' } })],
                next_before: 0,
              },
            };
          }
          return req.query.get('before')
            ? { body: { events: [ev(3, { data: { library: 'Kids', count: 1 } })], next_before: 0 } }
            : {
                body: {
                  events: [ev(9, { kind: 'library_unavailable', data: { library: 'Lectures' } })],
                  next_before: 9,
                },
              };
        },
      }),
    );
    renderApp('/server/events');
    const list = await screen.findByRole('region', { name: 'Events' });
    expect(within(list).getByText('Lectures is offline')).toBeInTheDocument();
    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: 'Show older' }));
    expect(await within(list).findByText('1 new book in Kids')).toBeInTheDocument();
    expect(calls.find((c) => c.query.get('before') === '9')?.query.get('limit')).toBe('50');

    await user.selectOptions(screen.getByRole('combobox', { name: 'Kind of event' }), 'new_device');
    expect(await screen.findByText('New sign-in: sam')).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByText('Lectures is offline')).not.toBeInTheDocument());
  });

  it('says when nothing of a kind happened', async () => {
    mockFetch(signedInRoutes({ 'GET /admin/events': { body: { events: [], next_before: 0 } } }));
    renderApp('/server/events');
    expect(await screen.findByText('Nothing yet')).toBeInTheDocument();
    await userEvent
      .setup()
      .selectOptions(screen.getByRole('combobox', { name: 'Kind of event' }), 'backup_failed');
    expect(await screen.findByText('None of these in the last 90 days')).toBeInTheDocument();
  });
});
