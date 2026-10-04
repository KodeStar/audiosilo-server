import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { setToken } from '@/api/token';
import type { AuditEvent } from '@/api/types';
import { mockFetch } from '@/test/fetch-mock';
import { renderApp } from '@/test/render-app';
import { signedInRoutes } from '@/test/routes';

// Server > Audit log: actions in words, details, filters, paging.

const ev = (over: Partial<AuditEvent>): AuditEvent => ({
  id: 1,
  at: '2026-10-04T15:01:00.000Z',
  actor_id: 1,
  actor_name: 'chris',
  via: 'session',
  action: 'user.update',
  target: 'sam',
  details: {},
  ...over,
});

beforeEach(() => setToken('stored'));

describe('audit log', () => {
  it('words each action, its target and details, and pages', async () => {
    const calls = mockFetch(
      signedInRoutes({
        'GET /admin/audit': (req) =>
          req.query.get('before')
            ? {
                body: {
                  events: [ev({ id: 1, action: 'user.create', target: 'old' })],
                  next_before: 0,
                },
              }
            : {
                body: {
                  events: [
                    ev({
                      id: 3,
                      action: 'user.update',
                      details: { password: 'set', disabled: true },
                    }),
                    ev({
                      id: 2,
                      action: 'backup.restore_applied',
                      via: 'system',
                      actor_id: null,
                      actor_name: '',
                      target: 'audiosilo-a.db',
                    }),
                  ],
                  next_before: 2,
                },
              },
      }),
    );
    renderApp('/server/audit');
    const list = await screen.findByRole('region', { name: 'Audit log' });
    const rows = within(list).getAllByRole('listitem');
    expect(rows[0]).toHaveTextContent('Changed an account');
    expect(rows[0]).toHaveTextContent('sam');
    expect(rows[0]).toHaveTextContent('Password: set');
    expect(rows[0]).toHaveTextContent('Disabled: yes');
    expect(rows[1]).toHaveTextContent('AudioSilo');
    expect(rows[1]).toHaveTextContent('Restored a backup');
    await userEvent.setup().click(screen.getByRole('button', { name: 'Show older' }));
    expect(await screen.findByText('Created an account')).toBeInTheDocument();
    expect(
      calls
        .filter((c) => c.path === '/admin/audit')
        .at(-1)
        ?.query.get('before'),
    ).toBe('2');
  });

  it('narrows to an area and says when nothing matches', async () => {
    const calls = mockFetch(
      signedInRoutes({ 'GET /admin/audit': { body: { events: [], next_before: 0 } } }),
    );
    renderApp('/server/audit');
    expect(await screen.findByText('Nothing recorded yet')).toBeInTheDocument();
    await userEvent.setup().selectOptions(screen.getByRole('combobox', { name: 'Area' }), 'backup');
    await waitFor(() =>
      expect(
        calls
          .filter((c) => c.path === '/admin/audit')
          .at(-1)
          ?.query.get('area'),
      ).toBe('backup'),
    );
    expect(await screen.findByText('Nothing matches')).toBeInTheDocument();
  });
});
