import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { setToken } from '@/api/token';
import type {
  AdminLibrary,
  LibraryPreview,
  SetupEnvelope,
  SetupPatch,
  SetupState,
} from '@/api/types';
import { mockFetch, type MockRoute } from '@/test/fetch-mock';
import { libraries, settingsWith, setupEnvelope, systemStatus } from '@/test/fixtures';
import { renderApp } from '@/test/render-app';
import { signedInRoutes } from '@/test/routes';

// The first-run flow (FIRST-RUN-PLAN.md Phase F1) against a mocked server:
// the redirect, the folder check, the naming pick, the library created after
// the metadata step, a locked setting, and Finish.

beforeEach(() => setToken('stored'));
afterEach(() => vi.unstubAllGlobals());

const names = (o: Partial<LibraryPreview['samples'][number]['tags']> = {}) => ({
  title: '',
  author: '',
  narrator: '',
  series: '',
  position: '',
  ...o,
});

const preview: LibraryPreview = {
  root: '/audiobooks',
  counts: { audio_files: 812, folder_books: 64, file_books: 3, capped: false },
  same_root: null,
  samples: [
    {
      rel_path: 'Pratchett/Discworld/04 - Mort',
      tags: names({ title: 'Mort' }),
      path: names({ title: 'Mort', author: 'Pratchett', series: 'Discworld', position: '4' }),
      abs: false,
    },
  ],
  blanks: { tags: 4, path: 1 },
  suggested: 'path',
};

/** The setup routes: the state kept in memory, PUT merging into it. */
function flowRoutes(
  start: Partial<SetupState> = {},
  over: Record<string, MockRoute> = {},
  env: Partial<SetupEnvelope> = {},
) {
  const state: SetupState = { finished: false, dismissed: false, steps: {}, ...start };
  return signedInRoutes({
    'GET /admin/setup': () => ({ body: setupEnvelope(state, env) }),
    'PUT /admin/setup': (req) => {
      const p = req.body as SetupPatch;
      Object.assign(state, { ...p, steps: { ...state.steps, ...p.steps } });
      return { body: state };
    },
    'POST /admin/setup/preview': { body: preview },
    'GET /admin/libraries': { body: { libraries: [] } },
    'GET /admin/system': { body: systemStatus() },
    ...over,
  });
}

const puts = (calls: ReturnType<typeof mockFetch>) =>
  calls.filter((c) => c.method === 'PUT' && c.path === '/admin/setup').map((c) => c.body);

describe('the first-run redirect', () => {
  it('sends an unfinished server to the flow, outside the shell, once a session', async () => {
    mockFetch(flowRoutes());
    const { router } = renderApp('/');
    await waitFor(() => expect(router.state.location.pathname).toBe('/setup'));
    expect(await screen.findByRole('heading', { name: 'Where are your books?' })).toBeVisible();
    // No top bar: the flow is a page of its own.
    expect(screen.queryAllByRole('navigation', { name: 'Primary' })).toEqual([]);

    await userEvent.setup().click(screen.getByRole('link', { name: 'Finish later' }));
    await waitFor(() => expect(router.state.location.pathname).toBe('/'));
    await screen.findAllByRole('navigation', { name: 'Primary' });
    expect(router.state.location.pathname).toBe('/');
  });

  it('leaves a finished server alone', async () => {
    mockFetch(signedInRoutes());
    const { router } = renderApp('/');
    await screen.findByRole('region', { name: 'Server' });
    expect(router.state.location.pathname).toBe('/');
  });
});

describe('the folder step', () => {
  it('checks the folder and says what it found', async () => {
    const calls = mockFetch(flowRoutes({}, {}, { mount_hints: ['/audiobooks', '/data'] }));
    renderApp('/setup');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: '/audiobooks' }));
    expect(await screen.findByText('Found 812 audio files')).toBeVisible();
    expect(screen.getByText(/Book folders: 64/)).toBeVisible();
    expect(screen.getByLabelText('Library name')).toHaveValue('audiobooks');
    const post = calls.find((c) => c.path === '/admin/setup/preview');
    expect(post?.body).toEqual({ root: '/audiobooks' });
  });

  it('words an unreadable folder with the Docker hint', async () => {
    mockFetch(
      flowRoutes(
        {},
        {
          'POST /admin/setup/preview': {
            status: 404,
            body: { error: 'folder not found or not readable', code: 'folder_unreadable' },
          },
        },
      ),
    );
    renderApp('/setup');
    const user = userEvent.setup();
    await user.type(await screen.findByLabelText('Books folder'), '/Users/me/Audiobooks');
    await user.click(screen.getByRole('button', { name: 'Check folder' }));
    expect(
      await screen.findByText(
        /\/Users\/me\/Audiobooks isn't there, or the server can't read it\. In Docker/,
      ),
    ).toBeVisible();
  });

  it('refuses a relative path before asking the server', async () => {
    const calls = mockFetch(flowRoutes());
    renderApp('/setup');
    const user = userEvent.setup();
    await user.type(await screen.findByLabelText('Books folder'), 'audiobooks');
    await user.click(screen.getByRole('button', { name: 'Check folder' }));
    expect(await screen.findByText(/Type the whole path/)).toBeVisible();
    expect(calls.some((c) => c.path === '/admin/setup/preview')).toBe(false);
  });

  it('lists config libraries as already added', async () => {
    mockFetch(
      flowRoutes(
        {},
        { 'GET /admin/libraries': { body: { libraries: libraries() } } },
        { config_libraries: ['Fiction'] },
      ),
    );
    renderApp('/setup');
    const added = await screen.findByRole('region', { name: 'Already added' });
    const rows = within(added).getAllByRole('listitem');
    expect(within(rows[0]).getByText('From your config')).toBeVisible();
    expect(within(rows[1]).queryByText('From your config')).not.toBeInTheDocument();
  });
});

describe('folder, naming and metadata', () => {
  it('creates the library with the chosen source only after the metadata step', async () => {
    let created: AdminLibrary[] = [];
    const calls = mockFetch(
      flowRoutes(
        {},
        {
          'GET /admin/libraries': () => ({ body: { libraries: created } }),
          'POST /admin/libraries': (req) => {
            const body = req.body as { name: string; root: string };
            created = libraries([{ ...body, id: 7 }]).slice(0, 1);
            return { status: 201, body: created[0] };
          },
        },
      ),
    );
    renderApp('/setup');
    const user = userEvent.setup();
    await user.type(await screen.findByLabelText('Books folder'), '/audiobooks');
    await user.click(screen.getByRole('button', { name: 'Continue' }));

    // Naming: the samples side by side, the side with fewer blanks preselected.
    expect(await screen.findByRole('heading', { name: 'How are your books named?' })).toBeVisible();
    const samples = screen.getByRole('list', { name: 'Sample books' });
    expect(within(samples).getByText('Pratchett/Discworld/04 - Mort')).toBeVisible();
    expect(screen.getByRole('radio', { name: /Folder names first/ })).toBeChecked();
    expect(calls.some((c) => c.path === '/admin/libraries' && c.method === 'POST')).toBe(false);
    await user.click(screen.getByRole('button', { name: 'Continue' }));

    // Metadata: nothing changed, so no settings PATCH; the library is created now.
    expect(await screen.findByRole('heading', { name: 'Community metadata' })).toBeVisible();
    await user.click(screen.getByRole('button', { name: 'Continue' }));
    expect(
      await screen.findByRole('heading', { name: 'How will people reach the server?' }),
    ).toBeVisible();
    const post = calls.find((c) => c.path === '/admin/libraries' && c.method === 'POST');
    expect(post?.body).toEqual({
      kind: 'books',
      name: 'audiobooks',
      root: '/audiobooks',
      metadata_source: 'path',
    });
    expect(calls.some((c) => c.method === 'PATCH' && c.path === '/admin/settings')).toBe(false);
    expect(puts(calls)).toEqual([
      { steps: { folder: 'done' } },
      { steps: { naming: 'done' } },
      { steps: { metadata: 'done' } },
    ]);
    // The scan shows from here on.
    expect(await screen.findByRole('region', { name: 'Library scans' })).toBeVisible();
  });

  it('keeps the pick made for each library the naming step offers', async () => {
    localStorage.setItem(
      'audiosilo.setup.draft',
      JSON.stringify({ root: '/audiobooks', name: 'Audiobooks' }),
    );
    const calls = mockFetch(
      flowRoutes(
        { steps: { folder: 'done' } },
        {
          'GET /admin/libraries': { body: { libraries: libraries() } },
          'PATCH /admin/libraries/1': (req) => ({
            body: { ...libraries()[0], ...(req.body as object) },
          }),
        },
        { config_libraries: ['Fiction'] },
      ),
    );
    renderApp('/setup?step=naming');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('radio', { name: /The files' tags first/ }));
    // Beside a draft, only config's libraries: Kids was made in the console.
    expect(screen.queryByRole('option', { name: /Kids/ })).toBeNull();
    await user.selectOptions(screen.getByLabelText('Library'), 'Fiction (from your config)');
    // An existing library opens on its own source, the preview's side a hint.
    expect(await screen.findByRole('radio', { name: /The files' tags first/ })).toBeChecked();
    expect(await screen.findByText(/Folder names look more complete/)).toBeVisible();
    await user.click(screen.getByRole('radio', { name: /Folder names first/ }));
    await user.click(screen.getByRole('button', { name: 'Continue' }));
    await screen.findByRole('heading', { name: 'Community metadata' });
    const patch = calls.find((c) => c.method === 'PATCH' && c.path === '/admin/libraries/1');
    expect(patch?.body).toEqual({ metadata_source: 'path' });
    expect(JSON.parse(localStorage.getItem('audiosilo.setup.draft') ?? '{}')).toEqual({
      root: '/audiobooks',
      name: 'Audiobooks',
      source: 'tags',
    });
  });

  it('with no draft, names a books library that exists already and switches its source', async () => {
    // A library the older setup page's fields made: not from config, no draft here.
    const calls = mockFetch(
      flowRoutes(
        { steps: { folder: 'done' } },
        {
          'GET /admin/libraries': { body: { libraries: libraries().slice(0, 1) } },
          'PATCH /admin/libraries/1': (req) => ({
            body: { ...libraries()[0], ...(req.body as object) },
          }),
        },
      ),
    );
    renderApp('/setup');
    expect(await screen.findByRole('heading', { name: 'How are your books named?' })).toBeVisible();
    expect(await screen.findByRole('radio', { name: /The files' tags first/ })).toBeChecked();
    const user = userEvent.setup();
    await user.click(screen.getByRole('radio', { name: /Folder names first/ }));
    expect(screen.getByText(/re-reads the details of its 2,400 books/)).toBeVisible();
    await user.click(screen.getByRole('button', { name: 'Continue' }));
    await screen.findByRole('heading', { name: 'Community metadata' });
    const preview = calls.find((c) => c.path === '/admin/setup/preview');
    expect(preview?.body).toEqual({ root: libraries()[0].root });
    const patch = calls.find((c) => c.method === 'PATCH' && c.path === '/admin/libraries/1');
    expect(patch?.body).toEqual({ metadata_source: 'path' });
    expect(puts(calls)).toEqual([{ steps: { naming: 'done' } }]);
  });

  it("plain Continue keeps an existing library's source, whatever the preview suggests", async () => {
    const calls = mockFetch(
      flowRoutes(
        { steps: { folder: 'done' } },
        { 'GET /admin/libraries': { body: { libraries: libraries().slice(0, 1) } } },
      ),
    );
    renderApp('/setup?step=naming');
    expect(await screen.findByRole('radio', { name: /The files' tags first/ })).toBeChecked();
    expect(await screen.findByText(/Folder names look more complete/)).toBeVisible();
    // The Suggested badge is the new library's; an existing one gets the hint.
    expect(screen.queryByText('Suggested')).toBeNull();
    await userEvent.setup().click(screen.getByRole('button', { name: 'Continue' }));
    await screen.findByRole('heading', { name: 'Community metadata' });
    expect(calls.some((c) => c.method === 'PATCH' && c.path.startsWith('/admin/libraries'))).toBe(
      false,
    );
    expect(puts(calls)).toEqual([{ steps: { naming: 'done' } }]);
  });

  it('suggests no side when the preview read no samples', async () => {
    mockFetch(
      flowRoutes(
        { steps: { folder: 'done' } },
        {
          'GET /admin/libraries': { body: { libraries: libraries().slice(0, 1) } },
          'POST /admin/setup/preview': {
            body: { ...preview, samples: [], blanks: { tags: 0, path: 0 }, suggested: 'tags' },
          },
        },
      ),
    );
    renderApp('/setup?step=naming');
    expect(await screen.findByRole('radio', { name: /The files' tags first/ })).toBeChecked();
    expect(await screen.findByText('No books to show from this folder.')).toBeVisible();
    expect(screen.queryByText(/look more complete/)).toBeNull();
  });

  it('a skipped folder creates nothing', async () => {
    const calls = mockFetch(flowRoutes());
    renderApp('/setup');
    await userEvent.setup().click(await screen.findByRole('button', { name: 'Skip' }));
    expect(await screen.findByText('No folder to name yet')).toBeVisible();
    expect(puts(calls)).toEqual([{ steps: { folder: 'skipped' } }]);
  });

  it('Continue past naming with nothing left to name keeps it done', async () => {
    const calls = mockFetch(
      flowRoutes({ steps: { folder: 'done', naming: 'done', metadata: 'done' } }),
    );
    renderApp('/setup?step=naming');
    expect(await screen.findByText('No folder to name yet')).toBeVisible();
    await userEvent.setup().click(screen.getByRole('button', { name: 'Continue' }));
    await waitFor(() => expect(puts(calls)).toEqual([{ steps: { naming: 'done' } }]));
  });
});

describe('the metadata step', () => {
  it('leaves the switch as saved when the service is not configured', async () => {
    const off = settingsWith();
    off.metadata = { ...off.metadata, enabled: true, available: false };
    const calls = mockFetch(
      flowRoutes(
        {},
        {
          'GET /admin/settings': { body: off },
          'PATCH /admin/settings': (req) => ({ body: settingsWith(req.body as object) }),
        },
      ),
    );
    renderApp('/setup?step=metadata');
    expect(
      await screen.findByRole('switch', { name: 'Look up community metadata' }),
    ).not.toBeChecked();
    await userEvent.setup().click(screen.getByRole('button', { name: 'Continue' }));
    await screen.findByRole('heading', { name: 'How will people reach the server?' });
    expect(calls.some((c) => c.method === 'PATCH' && c.path === '/admin/settings')).toBe(false);
  });
});

describe('the reach step', () => {
  // The page's own address decides what the step offers: jsdom's URL, moved
  // to a home-network one for a test and back after.
  const jsdom = (globalThis as unknown as { jsdom: { reconfigure(o: { url: string }): void } })
    .jsdom;
  const testUrl = window.location.href;
  const browseAt = (url: string) => jsdom.reconfigure({ url });
  afterEach(() => browseAt(testUrl));

  it('shows a setting the environment sets as set by your config, and leaves it out', async () => {
    const calls = mockFetch(
      flowRoutes(
        {},
        {
          'GET /admin/settings': {
            body: settingsWith({ locked: { 'general.public_url': 'AUDIOSILO_PUBLIC_URL' } }),
          },
          'PATCH /admin/settings': (req) => ({ body: settingsWith(req.body as object) }),
        },
      ),
    );
    browseAt('http://192.168.1.20:8080/admin/setup');
    renderApp('/setup?step=reach');
    expect(await screen.findByLabelText('Public address')).toBeDisabled();
    // One indicator per locked row: the words, the variable in its tooltip.
    const locked = screen.getAllByText('Set by your config');
    expect(locked).toHaveLength(1);
    expect(locked[0].closest('[title]')).toHaveAttribute('title', 'Set by AUDIOSILO_PUBLIC_URL');
    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: 'Use as the home address' }));
    await user.click(screen.getByRole('button', { name: 'Continue' }));
    await screen.findByRole('heading', { name: 'Coming from Audiobookshelf?' });
    const patch = calls.find((c) => c.method === 'PATCH' && c.path === '/admin/settings');
    expect(patch?.body).toEqual({ general: { lan_url: 'http://192.168.1.20:8080' } });
  });

  it('never offers a loopback address (localhost) as the home or public address', async () => {
    mockFetch(flowRoutes());
    browseAt('http://localhost:8080/admin/setup');
    renderApp('/setup?step=reach');
    expect(await screen.findByText('http://localhost:8080')).toBeVisible();
    expect(screen.queryByRole('button', { name: 'Use as the home address' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Use as the public address' })).toBeNull();
  });

  it('offers a public address only as the public one', async () => {
    mockFetch(flowRoutes());
    browseAt('https://books.example.com/admin/setup');
    renderApp('/setup?step=reach');
    expect(await screen.findByRole('button', { name: 'Use as the public address' })).toBeVisible();
    expect(screen.queryByRole('button', { name: 'Use as the home address' })).toBeNull();
  });

  it('offers an untrusted reverse proxy for trusted proxies', async () => {
    const calls = mockFetch(
      flowRoutes(
        {},
        { 'PATCH /admin/settings': { body: settingsWith() } },
        {
          request: {
            host: 'books.example.com',
            scheme: 'https',
            proxied: true,
            peer: '172.18.0.2',
            peer_trusted: false,
          },
        },
      ),
    );
    renderApp('/setup?step=reach');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Trust 172.18.0.2' }));
    await user.click(screen.getByRole('button', { name: 'Continue' }));
    await screen.findByRole('heading', { name: 'Coming from Audiobookshelf?' });
    const patch = calls.find((c) => c.method === 'PATCH' && c.path === '/admin/settings');
    expect(patch?.body).toEqual({ network: { trusted_proxies: ['172.18.0.2'] } });
  });
});

describe('the end of the flow', () => {
  const bridgeUnpaired = {
    paired: false,
    paired_by: null,
    pairing: null,
    online: false,
    kinds: [],
    features: [],
    libraries: [],
    workers: [],
  };

  it('Finish on the Bridge step marks the flow finished and opens Overview', async () => {
    const calls = mockFetch(
      flowRoutes(
        {},
        {
          'GET /admin/bridge': { body: bridgeUnpaired },
        },
      ),
    );
    const { router } = renderApp('/setup?step=bridge');
    expect(
      await screen.findByText(/image: ghcr.io\/kodestar\/audiosilo-bridge:latest/),
    ).toBeVisible();
    await userEvent.setup().click(screen.getByRole('button', { name: 'Finish setup' }));
    await waitFor(() => expect(router.state.location.pathname).toBe('/'));
    // Reached by a jump: every step without a mark goes down as skipped,
    // the folder too, since no library was made.
    expect(puts(calls)).toEqual([
      {
        steps: {
          folder: 'skipped',
          naming: 'skipped',
          metadata: 'skipped',
          reach: 'skipped',
          abs: 'skipped',
          backups: 'skipped',
          phone: 'skipped',
          bridge: 'skipped',
        },
        finished: true,
      },
    ]);
    const card = await screen.findByRole('region', { name: 'Finish setting up' });
    expect(within(card).getByRole('link', { name: /Where are your books/ })).toBeVisible();
    expect(within(card).getByRole('link', { name: /Add a Bridge/ })).toBeVisible();
  });

  it('Finish keeps the marks already made and skips only the unmarked steps', async () => {
    const calls = mockFetch(
      flowRoutes(
        { steps: { folder: 'done', naming: 'done', metadata: 'done', reach: 'skipped' } },
        { 'GET /admin/bridge': { body: bridgeUnpaired } },
      ),
    );
    const { router } = renderApp('/setup?step=bridge');
    await screen.findByText(/image: ghcr.io\/kodestar\/audiosilo-bridge:latest/);
    await userEvent.setup().click(screen.getByRole('button', { name: 'Finish setup' }));
    await waitFor(() => expect(router.state.location.pathname).toBe('/'));
    expect(puts(calls)).toEqual([
      {
        steps: { abs: 'skipped', backups: 'skipped', phone: 'skipped', bridge: 'skipped' },
        finished: true,
      },
    ]);
    const card = await screen.findByRole('region', { name: 'Finish setting up' });
    expect(within(card).queryByRole('link', { name: /Where are your books/ })).toBeNull();
    expect(within(card).getByRole('link', { name: /reach the server/ })).toBeVisible();
  });
});
