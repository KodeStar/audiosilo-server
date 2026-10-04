import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { setToken } from '@/api/token';
import { mockFetch, type MockRoute } from '@/test/fetch-mock';
import { systemStatus, updateStatus } from '@/test/fixtures';
import { renderApp } from '@/test/render-app';
import { signedInRoutes } from '@/test/routes';

// Health > System and Server > About: what the server depends on, and updates.

function routes(over: Record<string, MockRoute> = {}) {
  return signedInRoutes({ 'GET /admin/system': { body: systemStatus() }, ...over });
}

beforeEach(() => setToken('stored'));

describe('system', () => {
  it('lists every dependency with a status', async () => {
    mockFetch(routes());
    renderApp('/health/system');
    expect(
      await screen.findByText('Everything Hearthside depends on, in one place.'),
    ).toBeInTheDocument();
    const list = screen.getByRole('region', { name: 'System' });
    expect(within(list).getAllByRole('listitem')).toHaveLength(8);
    expect(within(list).getByText('Responding in 84 ms.')).toBeInTheDocument();
    expect(within(list).getByText(/5\.4 TB free of 8 TB/)).toBeInTheDocument();
    expect(within(list).queryByText('Needs attention')).not.toBeInTheDocument();
  });

  it('calls out an offline library and a silent metadata service', async () => {
    mockFetch(
      routes({
        'GET /admin/system': {
          body: systemStatus({
            libraries: [
              { id: 1, name: 'Lectures', root: '/mnt/nas', available: false, disk: null },
            ],
            metadata: {
              enabled: true,
              available: true,
              base_url: 'https://meta.audiosilo.app',
              health: { reachable: false, latency_ms: 0, checked_at: '2026-10-04T09:00:00Z' },
            },
          }),
        },
      }),
    );
    renderApp('/health/system');
    expect(await screen.findByText("A library folder isn't reachable")).toBeInTheDocument();
    expect(screen.getByText(/Nothing was deleted/)).toBeInTheDocument();
    expect(screen.getByText("The community metadata service isn't responding")).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Metadata settings' })).toHaveAttribute(
      'href',
      '/admin/server?topic=metadata',
    );
  });

  it('shows an error with a retry when the status fails', async () => {
    mockFetch(routes({ 'GET /admin/system': { status: 500, body: { error: 'boom' } } }));
    renderApp('/health/system');
    expect(await screen.findByText("The system status couldn't load")).toBeInTheDocument();
  });
});

describe('about', () => {
  it('says how to update a Docker install when a release is out', async () => {
    mockFetch(
      routes({
        'GET /admin/system': {
          body: systemStatus({
            update: updateStatus({
              update_available: true,
              latest: {
                version: 'v1.16.0',
                name: 'v1.16.0',
                url: 'https://github.com/KodeStar/audiosilo-server/releases/tag/v1.16.0',
                published_at: '2026-10-03T10:00:00Z',
              },
            }),
          }),
        },
      }),
    );
    renderApp('/server/about');
    expect(await screen.findByText('AudioSilo v1.16.0 is available')).toBeInTheDocument();
    expect(screen.getByText(/audiosilo-server:1\.16\.0/)).toBeInTheDocument();
    expect(screen.getByRole('link', { name: /Release notes/ })).toHaveAttribute(
      'href',
      'https://github.com/KodeStar/audiosilo-server/releases/tag/v1.16.0',
    );
    expect(screen.getByText('Docker container')).toBeInTheDocument();
  });

  it('checks now and shows the answer', async () => {
    const calls = mockFetch(
      routes({
        'POST /admin/update/check': {
          body: updateStatus({ error: 'rate_limited' }),
        },
      }),
    );
    renderApp('/server/about');
    expect(await screen.findByText("You're up to date")).toBeInTheDocument();
    await userEvent.setup().click(screen.getByRole('button', { name: 'Check now' }));
    expect(await screen.findByText(/GitHub is limiting requests/)).toBeInTheDocument();
    expect(calls.some((c) => c.method === 'POST' && c.path === '/admin/update/check')).toBe(true);
  });

  it('offers to turn the check on while it is off', async () => {
    mockFetch(
      routes({
        'GET /admin/system': { body: systemStatus({ update: updateStatus({ enabled: false }) }) },
      }),
    );
    renderApp('/server/about');
    expect(await screen.findByText('The update check is off')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Check now' })).not.toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'General settings' })).toHaveAttribute(
      'href',
      '/admin/server',
    );
  });

  it("links the overview's version to an available update", async () => {
    mockFetch(
      routes({
        'GET /admin/update': {
          body: updateStatus({
            update_available: true,
            latest: { version: 'v1.16.0', name: '', url: 'https://github.com/x', published_at: '' },
          }),
        },
      }),
    );
    renderApp('/');
    expect(await screen.findByRole('link', { name: 'v1.16.0 available' })).toHaveAttribute(
      'href',
      '/admin/server/about',
    );
  });
});
