import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { setToken } from '@/api/token';
import type { BridgeEstimate, BridgeJob, BridgeStatus, ServerInfo } from '@/api/types';
import { mockFetch, type MockRequest, type MockRoute } from '@/test/fetch-mock';
import { serverInfo } from '@/test/fixtures';
import { bookDetail } from '@/test/library-fixtures';
import { renderApp } from '@/test/render-app';
import { signedInRoutes } from '@/test/routes';

// Bridge Phase 2b on the book page and Settings > Bridge: Share with the
// community after a run kept here, the line saying which setup step Generate
// waits on, the Setup card, and every control gone with the module off.

const BOOK_PATH = 'Brandon Sanderson/The Stormlight Archive/01 - The Way of Kings';
const URL = `/library/book?library=1&path=${encodeURIComponent(BOOK_PATH)}`;

function server(contributions: boolean): ServerInfo {
  return {
    ...serverInfo,
    capabilities: { ...serverInfo.capabilities, bridge: true, contributions },
    bridge: { version: '0.3.0', features: ['bridge', 'sidecars', 'meta', 'contribute'] },
  };
}

function estimate(over: Partial<BridgeEstimate> = {}): BridgeEstimate {
  return {
    kind: 'sidecars.generate',
    library_id: 1,
    path: BOOK_PATH,
    source: 'audio',
    duration_seconds: 163800,
    asr_hours: 11.4,
    agent_cost_usd: 18.2,
    subscription: false,
    agent_ready: true,
    can_contribute: true,
    figures: true,
    writable: true,
    can_describe: false,
    // Generated here, a run kept on this server; the community has neither.
    has: {
      community: { characters: false, recaps: false, description: false },
      local: { characters: true, recaps: true, description: false },
    },
    earlier: [],
    ...over,
  };
}

function job(over: Partial<BridgeJob> = {}): BridgeJob {
  return {
    id: 41,
    kind: 'sidecars.contribute',
    library_id: 1,
    path: BOOK_PATH,
    params: {},
    state: 'queued',
    progress: {},
    result: {},
    error: '',
    requested_by: { id: 1, username: 'chris' },
    priority: 0,
    attempts: 0,
    lease_until: null,
    cancel_requested: false,
    created_at: '2026-10-19T12:00:00Z',
    updated_at: '2026-10-19T12:00:00Z',
    claimed_at: null,
    finished_at: null,
    ...over,
  };
}

function status(over: Partial<BridgeStatus> = {}): BridgeStatus {
  return {
    paired: true,
    paired_at: '2026-10-15T09:00:00Z',
    paired_by: { id: 1, username: 'admin' },
    pairing: null,
    online: true,
    heartbeat_at: new Date().toISOString(),
    version: '0.3.0',
    kinds: ['bridge.verify', 'sidecars.generate', 'sidecars.contribute', 'meta.correct'],
    features: ['bridge', 'sidecars', 'meta'],
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
          checked_path: 'A/B',
          error: '',
        },
      },
    ],
    workers: [],
    ui_url: 'http://192.168.1.20:8090/',
    setup: {
      items: [
        { id: 'paired', done: true, required: true },
        { id: 'libraries', done: true, required: true },
        { id: 'asr', done: true, required: true, detail: 'local' },
        { id: 'agent', done: true, required: true },
        { id: 'budget', done: true, required: true },
        { id: 'github', done: true, required: false },
      ],
      can_contribute: true,
      github_login: 'octocat',
    },
    ...over,
  };
}

function routes(over: Record<string, MockRoute> = {}) {
  return signedInRoutes({
    'GET /server': { body: server(true) },
    'GET /admin/libraries/1/book': { body: bookDetail() },
    'GET /admin/bridge': { body: status() },
    'GET /admin/bridge/jobs': { body: { jobs: [] } },
    'GET /admin/bridge/estimate': { body: estimate() },
    ...over,
  });
}

const queued = (calls: MockRequest[]) =>
  calls.filter((c) => c.method === 'POST' && c.path === '/admin/bridge/jobs');

beforeEach(() => setToken('stored'));
afterEach(() => vi.unstubAllGlobals());

describe('share with the community', () => {
  it('queues sidecars.contribute for what the community lacks, in one click', async () => {
    const user = userEvent.setup();
    const calls = mockFetch(routes({ 'POST /admin/bridge/jobs': { status: 201, body: job() } }));
    renderApp(URL);
    const share = await screen.findByRole('button', { name: 'Share with the community' });
    expect(
      screen.getByText(/The characters and recaps generated here aren't in the community/),
    ).toBeInTheDocument();
    await user.click(share);
    await waitFor(() => expect(queued(calls)).toHaveLength(1));
    expect(queued(calls)[0].body).toEqual({
      kind: 'sidecars.contribute',
      library_id: 1,
      path: BOOK_PATH,
      params: { dimensions: ['characters', 'recaps'] },
    });
  });

  it('shows the filed issues once the share is done, and offers it no more', async () => {
    const issue = 'https://github.com/KodeStar/audiosilo-meta-community/issues/7';
    mockFetch(
      routes({
        'GET /admin/bridge/jobs': {
          body: {
            jobs: [
              job({
                state: 'done',
                finished_at: '2026-10-19T12:05:00Z',
                result: {
                  library_id: 1,
                  path: BOOK_PATH,
                  work_id: 'w',
                  contributed: { recaps: issue },
                  covered: ['characters'],
                },
              }),
            ],
          },
        },
        'GET /admin/bridge/estimate': {
          body: estimate({
            has: {
              community: { characters: true, recaps: false, description: false },
              local: { characters: true, recaps: true, description: false },
            },
          }),
        },
      }),
    );
    renderApp(URL);
    expect(await screen.findByText(/Shared with the community/)).toBeInTheDocument();
    expect(
      screen.getByRole('link', { name: /See the community issue for the recaps/ }),
    ).toHaveAttribute('href', issue);
    expect(
      screen.getByText('The community already had the characters, so nothing was filed for them.'),
    ).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Share with the community' })).toBeNull();
  });

  it("holds Generate back while a share works, saying why, with the share's status", async () => {
    mockFetch(
      routes({
        'GET /admin/bridge/jobs': { body: { jobs: [job({ state: 'running' })] } },
      }),
    );
    renderApp(URL);
    expect(
      await screen.findByText(/being shared with the community\. Generate again once/),
    ).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Regenerate' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Share with the community' })).toBeNull();
    expect(
      screen.getByRole('button', { name: 'Cancel sharing with the community' }),
    ).toBeInTheDocument();
  });

  it('offers nothing to share while the Bridge cannot contribute', async () => {
    mockFetch(
      routes({ 'GET /admin/bridge/estimate': { body: estimate({ can_contribute: false }) } }),
    );
    renderApp(URL);
    await screen.findByRole('button', { name: 'Regenerate' });
    expect(screen.queryByRole('button', { name: 'Share with the community' })).toBeNull();
  });
});

describe('with the contributions module off', () => {
  it('shows no Share or Suggest corrections control at all', async () => {
    const detail = bookDetail();
    detail.fields.author = { ...detail.fields.author, value: 'B. Sanderson', source: 'edited' };
    mockFetch(
      routes({
        'GET /server': { body: server(false) },
        'GET /admin/libraries/1/book': { body: detail },
      }),
    );
    renderApp(URL);
    await screen.findByRole('button', { name: 'Regenerate' });
    expect(screen.queryByRole('button', { name: 'Share with the community' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Suggest corrections' })).toBeNull();
    expect(screen.queryByRole('heading', { name: 'Share with the community' })).toBeNull();
  });
});

describe('why Generate is missing', () => {
  it("names the agent credential the Bridge's checklist still needs, with a link to its settings", async () => {
    const s = status();
    s.setup!.items = s.setup!.items.map((i) => (i.id === 'agent' ? { ...i, done: false } : i));
    mockFetch(
      routes({
        'GET /admin/bridge': { body: s },
        'GET /admin/bridge/estimate': {
          body: estimate({
            has: {
              community: { characters: false, recaps: false, description: false },
              local: { characters: false, recaps: false, description: false },
            },
          }),
        },
      }),
    );
    renderApp(URL);
    expect(
      await screen.findByText(/Generating needs the Bridge's agent credential set first\./),
    ).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Generate characters and recaps' })).toBeNull();
    const card = screen.getByRole('heading', { name: 'Characters and recaps', level: 4 })
      .parentElement!.parentElement!;
    expect(within(card).getByRole('link', { name: 'Open Bridge settings' })).toBeInTheDocument();
  });

  it("doesn't count a library the Bridge can write to when another one isn't", async () => {
    const s = status();
    s.setup!.items = s.setup!.items.map((i) =>
      i.id === 'libraries' ? { ...i, done: false, detail: '1 of 2 libraries writable' } : i,
    );
    mockFetch(routes({ 'GET /admin/bridge': { body: s } }));
    renderApp(URL);
    expect(await screen.findByRole('button', { name: 'Regenerate' })).toBeInTheDocument();
    expect(screen.queryByText(/Generating needs/)).toBeNull();
  });
});

describe('Settings > Bridge setup', () => {
  const SETTINGS = '/server?topic=bridge';

  it('lists the checklist, the GitHub account and the agent credential, read-only', async () => {
    const s = status();
    s.setup = {
      items: [
        { id: 'paired', done: true, required: true },
        { id: 'libraries', done: false, required: true, detail: '1 of 2 libraries writable' },
        { id: 'agent', done: false, required: true },
        { id: 'audible', done: false, required: false, detail: 'Coming later' },
        { id: 'github', done: false, required: false },
      ],
      can_contribute: false,
    };
    mockFetch(routes({ 'GET /admin/bridge': { body: s } }));
    renderApp(SETTINGS);
    // Required steps still to do: the card leads, titled as the Bridge's own.
    const card = (await screen.findByRole('heading', { name: 'Get set up' })).closest(
      'section, [aria-labelledby]',
    ) as HTMLElement;
    expect(within(card).getByText('1 of 3 required steps done')).toBeInTheDocument();
    const list = within(card).getByRole('list', { name: 'Setup steps' });
    expect(within(list).getByText('Libraries mapped and writable')).toBeInTheDocument();
    expect(within(list).getByText('1 of 2 libraries writable')).toBeInTheDocument();
    // A step this console doesn't know is still listed, with the Bridge's words.
    expect(within(list).getByText('Audible')).toBeInTheDocument();
    expect(within(list).getByText('Coming later')).toBeInTheDocument();
    expect(
      within(list).getByRole('link', {
        name: 'Fix Agent credential set on the Bridge (opens its page)',
      }),
    ).toHaveAttribute('href', 'http://192.168.1.20:8090/#/accounts');
    expect(within(card).getByText('Not signed in')).toBeInTheDocument();
    expect(within(card).getByText('Not set')).toBeInTheDocument();
    expect(within(card).getByRole('link', { name: /Open Accounts on the Bridge/ })).toHaveAttribute(
      'href',
      'http://192.168.1.20:8090/#/accounts',
    );
    // Nothing on the card signs in or out: that happens on the Bridge.
    expect(within(card).queryByRole('button')).toBeNull();
  });

  it('says when everything required is done, with the signed-in account', async () => {
    mockFetch(routes());
    renderApp(SETTINGS);
    expect(await screen.findByText('Everything required is done')).toBeInTheDocument();
    expect(screen.getByText('octocat')).toBeInTheDocument();
  });

  it('shows no setup card for an older Bridge that sends no checklist', async () => {
    mockFetch(routes({ 'GET /admin/bridge': { body: status({ setup: undefined }) } }));
    renderApp(SETTINGS);
    await screen.findByText('Last report');
    expect(screen.queryByRole('heading', { name: 'Setup' })).toBeNull();
    expect(screen.queryByRole('heading', { name: 'Get set up' })).toBeNull();
  });
});
