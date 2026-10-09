import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { setToken } from '@/api/token';
import { mockFetch, type MockRoute } from '@/test/fetch-mock';
import { mirrorStatus, mirrorSystem, systemStatus, updateStatus } from '@/test/fixtures';
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
              mode: 'remote',
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

describe('system in mirror mode', () => {
  it('shows the local copy under its row', async () => {
    mockFetch(routes({ 'GET /admin/system': { body: mirrorSystem() } }));
    renderApp('/health/system');
    expect(
      await screen.findByText(
        'Answering from the local copy. No book is looked up over the internet.',
      ),
    ).toBeInTheDocument();
    expect(screen.getByText('data-v2026.10.09-1a2b3c4-5d6e7f8')).toBeInTheDocument();
    expect(screen.getByText('1.7 GB')).toBeInTheDocument();
    expect(screen.getByText('Oct 9, 2026')).toBeInTheDocument();
    expect(screen.getByText('Next check')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Check now' })).toBeEnabled();
    // The service notice is remote mode's; the copy's row says what's going on.
    expect(screen.queryByText(/isn't responding/)).not.toBeInTheDocument();
    expect(screen.queryByText(/Using the online service/)).not.toBeInTheDocument();
  });

  it('follows a first download, with lookups online meanwhile', async () => {
    const downloading = (done: number) =>
      mirrorStatus({
        state: 'downloading',
        fallback: true,
        progress: { done, total: 440_000_000 },
        tag: undefined,
        next_check_at: undefined,
      });
    // The light route is what the page polls while the copy downloads.
    const calls = mockFetch(
      routes({
        'GET /admin/system': { body: mirrorSystem(downloading(110_000_000)) },
        'GET /admin/meta/mirror': { body: downloading(220_000_000) },
      }),
    );
    renderApp('/health/system');
    expect(
      await screen.findByText('Downloading the local copy for the first time.'),
    ).toBeInTheDocument();
    expect(
      await screen.findByText('220 MB of 440 MB downloaded (50%)', {}, { timeout: 3000 }),
    ).toBeInTheDocument();
    expect(screen.getByRole('progressbar', { name: 'Download progress' })).toHaveAttribute(
      'aria-valuenow',
      '50',
    );
    expect(calls.filter((c) => c.path === '/admin/system')).toHaveLength(1);
    expect(
      screen.getByText('Using the online service until the local copy is ready'),
    ).toBeInTheDocument();
    expect(screen.getByText(/Lookups go to meta\.audiosilo\.app meanwhile/)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Check now' })).toBeDisabled();
  });

  it('says why a download failed, and that a newer copy needs a newer server', async () => {
    mockFetch(
      routes({
        'GET /admin/system': {
          body: mirrorSystem(
            mirrorStatus({
              schema_newer: true,
              error: 'not enough disk space: need 3.1 GB, have 1.2 GB',
            }),
          ),
        },
      }),
    );
    renderApp('/health/system');
    expect(
      await screen.findByText('This copy is newer than this server understands'),
    ).toBeInTheDocument();
    expect(
      screen.getByText('The last update failed. The current copy stays in use.'),
    ).toBeInTheDocument();
    expect(screen.getByText('not enough disk space: need 3.1 GB, have 1.2 GB')).toBeInTheDocument();
  });

  it('fetches the whole status once when a download ends', async () => {
    let systemGets = 0;
    const calls = mockFetch(
      routes({
        'GET /admin/system': () => {
          systemGets++;
          return {
            body: mirrorSystem(
              systemGets === 1
                ? mirrorStatus({ state: 'downloading', progress: { done: 1, total: 4 } })
                : mirrorStatus({ tag: 'data-v2026.10.10-aaaaaaa-bbbbbbb' }),
            ),
          };
        },
        'GET /admin/meta/mirror': {
          body: mirrorStatus({ tag: 'data-v2026.10.10-aaaaaaa-bbbbbbb' }),
        },
      }),
    );
    renderApp('/health/system');
    expect(
      await screen.findByText('data-v2026.10.10-aaaaaaa-bbbbbbb', {}, { timeout: 3000 }),
    ).toBeInTheDocument();
    await waitFor(() => expect(systemGets).toBe(2));
    // Ready again: the light poll stops.
    const polls = calls.filter((c) => c.path === '/admin/meta/mirror').length;
    await new Promise((r) => setTimeout(r, 2200));
    expect(calls.filter((c) => c.path === '/admin/meta/mirror')).toHaveLength(polls);
  });

  it('checks now and follows the check', async () => {
    const calls = mockFetch(
      routes({
        'GET /admin/system': { body: mirrorSystem() },
        'POST /admin/meta/mirror/check': {
          status: 202,
          body: mirrorStatus({ next_check_at: new Date().toISOString() }),
        },
        'GET /admin/meta/mirror': {
          body: mirrorStatus({
            state: 'downloading',
            next_check_at: undefined,
            progress: { done: 0, total: 0 },
          }),
        },
      }),
    );
    renderApp('/health/system');
    await userEvent.setup().click(await screen.findByRole('button', { name: 'Check now' }));
    expect(await screen.findByText('Checking for a newer copy')).toBeInTheDocument();
    expect(calls.some((c) => c.method === 'POST' && c.path === '/admin/meta/mirror/check')).toBe(
      true,
    );
    // The answer says a check is due now: the page follows it on the light route,
    // which finds the download running.
    await waitFor(() => expect(screen.getByRole('button', { name: 'Check now' })).toBeDisabled(), {
      timeout: 3000,
    });
    expect(calls.some((c) => c.method === 'GET' && c.path === '/admin/meta/mirror')).toBe(true);
  });

  it.each([
    [409, 'not_mirror_mode', /isn't keeping a local copy/],
    [404, 'metadata_off', /Community metadata is off/],
  ])('says why a check was refused (%i %s)', async (status, code, why) => {
    mockFetch(
      routes({
        'GET /admin/system': { body: mirrorSystem() },
        'POST /admin/meta/mirror/check': { status, body: { error: 'the server says no', code } },
      }),
    );
    renderApp('/health/system');
    await userEvent.setup().click(await screen.findByRole('button', { name: 'Check now' }));
    expect(await screen.findByText("Couldn't check for a newer copy")).toBeInTheDocument();
    expect(screen.getByText(why)).toBeInTheDocument();
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
