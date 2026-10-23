import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { setToken } from '@/api/token';
import type { BridgeJob, BridgeStatus } from '@/api/types';
import { mockFetch, type MockRoute } from '@/test/fetch-mock';
import { renderApp } from '@/test/render-app';
import { signedInRoutes } from '@/test/routes';

// Settings > Bridge: pairing, the paired Bridge's libraries and jobs, unpairing.

const CODE = 'ABCD-EFGH-JKMN-PQRS';

function notPaired(over: Partial<BridgeStatus> = {}): BridgeStatus {
  return {
    paired: false,
    paired_by: null,
    pairing: null,
    online: false,
    kinds: [],
    features: [],
    libraries: [{ id: 1, name: 'Fiction', root: '/books/fiction', mapping: null }],
    workers: [],
    ...over,
  };
}

function paired(over: Partial<BridgeStatus> = {}): BridgeStatus {
  return {
    paired: true,
    paired_at: '2026-10-15T09:00:00Z',
    paired_by: { id: 1, username: 'admin' },
    pairing: null,
    online: true,
    heartbeat_at: new Date().toISOString(),
    version: '0.1.0',
    kinds: ['bridge.verify'],
    features: ['bridge'],
    libraries: [
      {
        id: 1,
        name: 'Fiction',
        root: '/books/fiction',
        mapping: {
          library_id: 1,
          mount_path: '/libraries/fiction',
          mapped: true,
          writable: true,
          checked_path: 'Le Guin/Earthsea/A Wizard of Earthsea',
          error: '',
        },
      },
      { id: 2, name: 'Lectures', root: '/books/lectures', mapping: null },
    ],
    workers: [],
    ui_url: 'http://192.168.1.20:8090/',
    ...over,
  };
}

function job(over: Partial<BridgeJob> = {}): BridgeJob {
  return {
    id: 7,
    kind: 'bridge.verify',
    library_id: 1,
    path: null,
    params: { hold_seconds: 60 },
    state: 'running',
    progress: { step: 'hold', message: 'Holding for 60 s' },
    result: {},
    error: '',
    requested_by: { id: 1, username: 'admin' },
    priority: 0,
    attempts: 1,
    lease_until: '2026-10-15T12:02:00Z',
    cancel_requested: false,
    created_at: '2026-10-15T12:00:00Z',
    updated_at: '2026-10-15T12:00:30Z',
    claimed_at: '2026-10-15T12:00:05Z',
    finished_at: null,
    ...over,
  };
}

function routes(over: Record<string, MockRoute> = {}) {
  return signedInRoutes({
    'GET /admin/bridge': { body: paired() },
    'GET /admin/bridge/jobs': { body: { jobs: [] } },
    ...over,
  });
}

beforeEach(() => setToken('stored'));

describe('bridge page', () => {
  it('pairs: shows the code once, then flips to paired when a Bridge takes it', async () => {
    // Not paired; once the code is made it waits; a later poll finds a Bridge paired.
    let made = false;
    let polls = 0;
    const calls = mockFetch(
      routes({
        'GET /admin/bridge': () => {
          if (!made) return { body: notPaired() };
          polls++;
          return polls < 2
            ? { body: notPaired({ pairing: { expires_at: '2099-01-01T00:00:00Z' } }) }
            : { body: paired() };
        },
        'POST /admin/bridge/pair': () => {
          made = true;
          return {
            status: 201,
            body: {
              code: CODE,
              expires_at: new Date(Date.now() + 15 * 60_000).toISOString(),
              server_url: 'https://books.example.com',
            },
          };
        },
      }),
    );
    renderApp('/server?topic=bridge');
    const user = userEvent.setup();
    expect(await screen.findByText(/optional companion/)).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Connect a Bridge' }));

    const dialog = await screen.findByRole('dialog', { name: 'Connect a Bridge' });
    expect(within(dialog).getAllByText(CODE, { exact: false }).length).toBeGreaterThan(0);
    expect(within(dialog).getByLabelText('Server address')).toHaveValue(
      'https://books.example.com',
    );
    expect(within(dialog).getByText(/Expires in 1[45]:\d\d/)).toBeInTheDocument();
    expect(within(dialog).getByText(/AUDIOSILO_BRIDGE_PAIR_CODE=ABCD/)).toBeInTheDocument();
    expect(
      calls.filter((c) => c.method === 'POST' && c.path === '/admin/bridge/pair'),
    ).toHaveLength(1);

    // The status poll (every 3 s while the code waits) sees the Bridge paired.
    expect(await screen.findByText('Bridge connected', {}, { timeout: 6000 })).toBeInTheDocument();
    await waitFor(() =>
      expect(screen.queryByRole('dialog', { name: 'Connect a Bridge' })).not.toBeInTheDocument(),
    );
    expect(await screen.findByRole('button', { name: 'Unpair...' })).toBeInTheDocument();
  }, 10_000);

  it('says when a code is waiting and offers a new one, with no Jobs card while there are none', async () => {
    const calls = mockFetch(
      routes({
        'GET /admin/bridge': {
          body: notPaired({
            pairing: { expires_at: new Date(Date.now() + 10 * 60_000).toISOString() },
          }),
        },
      }),
    );
    renderApp('/server?topic=bridge');
    expect(await screen.findByText('A pairing code is waiting for a Bridge')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Make a new code' })).toBeInTheDocument();
    await waitFor(() =>
      expect(calls.some((c) => c.method === 'GET' && c.path === '/admin/bridge/jobs')).toBe(true),
    );
    expect(screen.queryByRole('heading', { name: 'Jobs' })).not.toBeInTheDocument();
  });

  it('unpaired with a queued job: the Jobs card shows and Cancel posts', async () => {
    let cancelled = false;
    const calls = mockFetch(
      routes({
        'GET /admin/bridge': { body: notPaired() },
        'GET /admin/bridge/jobs': () => ({
          body: {
            jobs: [
              job({
                state: cancelled ? 'cancelled' : 'queued',
                progress: {},
                attempts: 0,
                lease_until: null,
                claimed_at: null,
              }),
            ],
          },
        }),
        'POST /admin/bridge/jobs/7/cancel': () => {
          cancelled = true;
          return { body: job({ state: 'cancelled', finished_at: '2026-10-15T12:01:00Z' }) };
        },
      }),
    );
    renderApp('/server?topic=bridge');
    const user = userEvent.setup();
    expect(await screen.findByRole('button', { name: 'Connect a Bridge' })).toBeInTheDocument();
    const jobs = await screen.findByRole('list', { name: 'Jobs' });
    expect(within(jobs).getByText('Queued')).toBeInTheDocument();
    expect(within(jobs).getByText('Fiction', { exact: false })).toBeInTheDocument();
    await user.click(within(jobs).getByRole('button', { name: 'Cancel job 7 (Verify)' }));
    await waitFor(() =>
      expect(
        calls.some((c) => c.method === 'POST' && c.path === '/admin/bridge/jobs/7/cancel'),
      ).toBe(true),
    );
    expect(await screen.findByText('Job cancelled')).toBeInTheDocument();
    expect(await within(jobs).findByText('Cancelled')).toBeInTheDocument();
  });

  it('shows the paired Bridge and its libraries, and Verify queues bridge.verify', async () => {
    const calls = mockFetch(
      routes({
        'POST /admin/bridge/jobs': { status: 201, body: job({ state: 'queued', params: {} }) },
      }),
    );
    renderApp('/server?topic=bridge');
    const user = userEvent.setup();
    const libs = await screen.findByRole('list', { name: 'Libraries' });
    expect(screen.getByText('Online')).toBeInTheDocument();
    expect(screen.getByText('0.1.0')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: /Open its page/ })).toHaveAttribute(
      'href',
      'http://192.168.1.20:8090/',
    );
    expect(screen.getByRole('link', { name: /Open its page/ })).toHaveAttribute(
      'rel',
      'noopener noreferrer',
    );
    expect(within(libs).getByText('/libraries/fiction')).toBeInTheDocument();
    expect(within(libs).getByText('Writable')).toBeInTheDocument();
    // A library the Bridge hasn't reported on yet.
    expect(within(libs).getAllByText('Not reported yet').length).toBeGreaterThan(0);
    expect(screen.getByText('No workers')).toBeInTheDocument();

    await user.click(within(libs).getAllByRole('button', { name: 'Verify' })[0]);
    await waitFor(() =>
      expect(
        calls.find((c) => c.method === 'POST' && c.path === '/admin/bridge/jobs')?.body,
      ).toEqual({ kind: 'bridge.verify', library_id: 1 }),
    );
    expect(await screen.findByText('Verify queued for Fiction')).toBeInTheDocument();
  });

  it('queues a verify with a hold from its options', async () => {
    const calls = mockFetch(
      routes({ 'POST /admin/bridge/jobs': { status: 201, body: job({ state: 'queued' }) } }),
    );
    renderApp('/server?topic=bridge');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Verify Lectures with options' }));
    const dialog = await screen.findByRole('dialog', { name: 'Verify Lectures' });
    await user.click(within(dialog).getByText('More options'));
    const hold = within(dialog).getByLabelText('Hold (seconds)');
    await user.clear(hold);
    await user.type(hold, '60');
    await user.click(within(dialog).getByRole('button', { name: 'Queue verify' }));
    await waitFor(() =>
      expect(
        calls.find((c) => c.method === 'POST' && c.path === '/admin/bridge/jobs')?.body,
      ).toEqual({ kind: 'bridge.verify', library_id: 2, params: { hold_seconds: 60 } }),
    );
  });

  it('says so when the job it would queue is still stopping after a cancel', async () => {
    const calls = mockFetch(
      routes({
        'POST /admin/bridge/jobs': { status: 200, body: job({ cancel_requested: true }) },
      }),
    );
    renderApp('/server?topic=bridge');
    const user = userEvent.setup();
    const libs = await screen.findByRole('list', { name: 'Libraries' });
    await user.click(within(libs).getAllByRole('button', { name: 'Verify' })[0]);
    await waitFor(() =>
      expect(calls.some((c) => c.method === 'POST' && c.path === '/admin/bridge/jobs')).toBe(true),
    );
    expect(await screen.findByText('Verify for Fiction is still stopping')).toBeInTheDocument();
    expect(screen.queryByText(/is already queued/)).not.toBeInTheDocument();
  });

  it("says so when the paired Bridge doesn't run the job", async () => {
    mockFetch(
      routes({
        'POST /admin/bridge/jobs': {
          status: 409,
          body: {
            error: "the paired bridge doesn't run bridge.verify",
            code: 'kind_unavailable',
          },
        },
      }),
    );
    renderApp('/server?topic=bridge');
    const user = userEvent.setup();
    const libs = await screen.findByRole('list', { name: 'Libraries' });
    await user.click(within(libs).getAllByRole('button', { name: 'Verify' })[0]);
    expect(await screen.findByText("Couldn't queue the job")).toBeInTheDocument();
    expect(screen.getByText(/The paired Bridge doesn't run this job/)).toBeInTheDocument();
  });

  it('cancels a running job, which then reads as cancelling', async () => {
    let asked = false;
    const calls = mockFetch(
      routes({
        'GET /admin/bridge/jobs': () => ({ body: { jobs: [job({ cancel_requested: asked })] } }),
        'POST /admin/bridge/jobs/7/cancel': () => {
          asked = true;
          return { body: job({ cancel_requested: true }) };
        },
      }),
    );
    renderApp('/server?topic=bridge');
    const user = userEvent.setup();
    const jobs = await screen.findByRole('list', { name: 'Jobs' });
    expect(within(jobs).getByText('Running')).toBeInTheDocument();
    expect(within(jobs).getByText('Holding for 60 s')).toBeInTheDocument();
    await user.click(within(jobs).getByRole('button', { name: 'Cancel job 7 (Verify)' }));
    await waitFor(() =>
      expect(
        calls.some((c) => c.method === 'POST' && c.path === '/admin/bridge/jobs/7/cancel'),
      ).toBe(true),
    );
    expect(await screen.findByText('Asked the Bridge to stop')).toBeInTheDocument();
    expect(await within(jobs).findByText('Cancelling')).toBeInTheDocument();
    expect(within(jobs).queryByRole('button', { name: /Cancel job/ })).not.toBeInTheDocument();
  });

  it('unpairs after saying what happens', async () => {
    let unpaired = false;
    const calls = mockFetch(
      routes({
        'GET /admin/bridge': () => ({ body: unpaired ? notPaired() : paired() }),
        'DELETE /admin/bridge': () => {
          unpaired = true;
          return { status: 204 };
        },
      }),
    );
    renderApp('/server?topic=bridge');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Unpair...' }));
    const dialog = await screen.findByRole('dialog', { name: 'Unpair the Bridge?' });
    expect(within(dialog).getByText(/Queued ones wait for the next Bridge/)).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Unpair' }));
    await waitFor(() =>
      expect(calls.some((c) => c.method === 'DELETE' && c.path === '/admin/bridge')).toBe(true),
    );
    expect(await screen.findByText('Bridge unpaired')).toBeInTheDocument();
    expect(await screen.findByRole('button', { name: 'Connect a Bridge' })).toBeInTheDocument();
  });

  it('shows the unpaired card how to get a Bridge: the compose snippet and the docs (ADMIN-05)', async () => {
    mockFetch(routes({ 'GET /admin/bridge': { body: notPaired() } }));
    renderApp('/server?topic=bridge');
    expect(
      await screen.findByText(/image: ghcr.io\/kodestar\/audiosilo-bridge:latest/),
    ).toBeInTheDocument();
    // One volume per library, at the server's own path.
    expect(screen.getByText(/\/path\/to\/fiction:\/books\/fiction:rw/)).toBeInTheDocument();
    expect(screen.getByRole('link', { name: /Running a Bridge/ })).toHaveAttribute(
      'href',
      'https://docs.audiosilo.app/users/getting-started/running-a-bridge',
    );
    expect(screen.getByText(/connect it with a pairing code/)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Connect a Bridge' })).toBeInTheDocument();
  });

  it("gives a loopback server address as the compose snippet's, saying why (ADMIN-10)", async () => {
    mockFetch(
      routes({
        'GET /admin/bridge': { body: notPaired() },
        'POST /admin/bridge/pair': {
          status: 201,
          body: {
            code: CODE,
            expires_at: new Date(Date.now() + 15 * 60_000).toISOString(),
            server_url: 'http://127.0.0.1:8080',
          },
        },
      }),
    );
    renderApp('/server?topic=bridge');
    await userEvent.setup().click(await screen.findByRole('button', { name: 'Connect a Bridge' }));
    const dialog = await screen.findByRole('dialog', { name: 'Connect a Bridge' });
    expect(within(dialog).getByLabelText('Server address')).toHaveValue('http://audiosilo:8080');
    expect(
      within(dialog).getByText(/only knows itself as http:\/\/127\.0\.0\.1:8080/),
    ).toBeVisible();
    expect(
      within(dialog).getByText(/AUDIOSILO_BRIDGE_SERVER_URL=http:\/\/audiosilo:8080/),
    ).toBeInTheDocument();
  });

  it('offers no Verify on a podcast library, and names its marker check (BRIDGE-04, BRIDGE-17)', async () => {
    const status = paired();
    status.libraries = [
      status.libraries[0],
      {
        id: 3,
        name: 'Podcasts',
        root: '/podcasts',
        kind: 'podcasts',
        mapping: {
          library_id: 3,
          mount_path: '/libraries/podcasts',
          mapped: true,
          writable: true,
          checked_path: '.audiosilo/library.json',
          error: '',
        },
      },
    ];
    mockFetch(routes({ 'GET /admin/bridge': { body: status } }));
    renderApp('/server?topic=bridge');
    const libs = await screen.findByRole('list', { name: 'Libraries' });
    const rows = within(libs).getAllByRole('listitem');
    const podcasts = rows.find((r) => within(r).queryByText('Podcasts')) as HTMLElement;
    const fiction = rows.find((r) => within(r).queryByText('Fiction')) as HTMLElement;
    expect(within(fiction).getByRole('button', { name: 'Verify' })).toBeInTheDocument();
    expect(within(podcasts).queryByRole('button', { name: 'Verify' })).toBeNull();
    expect(within(podcasts).getByText('Found by its marker')).toBeInTheDocument();
    expect(within(fiction).getByText('Checked with')).toBeInTheDocument();
  });

  it("says a job the server queued itself is the server's, not a deleted account's (BRIDGE-03)", async () => {
    mockFetch(
      routes({
        'GET /admin/bridge/jobs': {
          body: {
            jobs: [
              job({ id: 8, kind: 'podcast.poll', requested_by: null, state: 'done' }),
              job({ id: 9, requested_by: null, state: 'done' }),
            ],
          },
        },
      }),
    );
    renderApp('/server?topic=bridge');
    expect(await screen.findByText(/Scheduled by the server/)).toBeInTheDocument();
    expect(screen.getAllByText(/Asked by a deleted account/)).toHaveLength(1);
  });
});
