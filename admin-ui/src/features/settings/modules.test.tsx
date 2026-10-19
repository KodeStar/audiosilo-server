import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { setToken } from '@/api/token';
import type { AdminModule, AdminSettings } from '@/api/types';
import { mockFetch, type MockRoute } from '@/test/fetch-mock';
import { settingsWith, systemStatus } from '@/test/fixtures';
import { renderApp } from '@/test/render-app';
import { signedInRoutes } from '@/test/routes';
import { MODULE_PAGES } from './settings-model';

// Settings > Modules (Bridge Phase 2a): a card per module with its switch, its
// Bridge state, the note while it is on but not yet offered, and the lock the
// environment puts on it.

function contributions(over: Partial<AdminModule> = {}): AdminModule {
  return {
    id: 'contributions',
    enabled: false,
    active: false,
    requires_bridge: 'contribute',
    bridge_state: 'offline',
    ...over,
  };
}

function routes(modules: AdminModule[], over: Record<string, MockRoute> = {}) {
  return signedInRoutes({
    'GET /admin/system': { body: systemStatus() },
    'GET /admin/modules': { body: { modules } },
    ...over,
  });
}

/** A PATCH that answers with the settings merged with what it sent. */
function patchEcho(base: AdminSettings): MockRoute {
  return (req) => {
    const body = req.body as Record<string, Record<string, unknown>>;
    const next = { ...base } as unknown as Record<string, unknown>;
    for (const [section, fields] of Object.entries(body)) {
      next[section] = { ...(next[section] as object), ...fields };
    }
    return { body: next };
  };
}

async function card() {
  const heading = await screen.findByRole('heading', { name: 'Contributions' });
  return heading.closest('section') as HTMLElement;
}

beforeEach(() => setToken('stored'));

describe('settings > modules', () => {
  it('is a topic in the nav, after Bridge', async () => {
    mockFetch(routes([contributions()]));
    const { router } = renderApp('/server');
    const nav = await screen.findByRole('navigation', { name: 'Settings topics' });
    const links = within(nav)
      .getAllByRole('link')
      .map((l) => l.textContent);
    expect(links.indexOf('Modules')).toBe(links.indexOf('Bridge') + 1);
    await userEvent.setup().click(within(nav).getByRole('link', { name: 'Modules' }));
    await waitFor(() => expect(router.state.location.search).toEqual({ topic: 'modules' }));
    expect(await card()).toBeInTheDocument();
  });

  it('shows an off module needing a Bridge, with its blurb and docs link', async () => {
    mockFetch(routes([contributions()]));
    renderApp('/server?topic=modules');
    const c = await card();
    expect(within(c).getByText(/Arrives in a later release/)).toBeInTheDocument();
    const docs = within(c).getByRole('link', { name: /Learn more about Contributions/ });
    expect(docs).toHaveAttribute('href', 'https://docs.audiosilo.app/users/admin/server#modules');
    expect(docs).toHaveAttribute('target', '_blank');
    expect(within(c).getByRole('switch', { name: 'Enable Contributions' })).not.toBeChecked();
    expect(within(c).getByText('Needs a Bridge')).toBeInTheDocument();
    expect(within(c).getByRole('link', { name: 'Bridge settings' })).toHaveAttribute(
      'href',
      '/admin/server?topic=bridge',
    );
    expect(within(c).queryByText(/hidden from listeners/)).not.toBeInTheDocument();
  });

  it("says what the Bridge lacks, in the module's own words", async () => {
    mockFetch(routes([contributions({ bridge_state: 'no_feature' })]));
    renderApp('/server?topic=modules');
    const c = await card();
    expect(within(c).getByText('Sign in to GitHub on your Bridge')).toBeInTheDocument();
    expect(within(c).getByRole('link', { name: 'Bridge settings' })).toBeInTheDocument();
  });

  it('says the Bridge is ready, with no link to set it up', async () => {
    mockFetch(routes([contributions({ bridge_state: 'ready' })]));
    renderApp('/server?topic=modules');
    const c = await card();
    expect(within(c).getByText('Bridge ready')).toBeInTheDocument();
    expect(within(c).queryByRole('link', { name: 'Bridge settings' })).not.toBeInTheDocument();
  });

  it('notes an enabled module still hidden from listeners, and not one that is offered', async () => {
    mockFetch(
      routes([contributions({ enabled: true, bridge_state: 'no_feature' })], {
        'GET /admin/settings': { body: settingsWith({ modules: { contributions: true } }) },
      }),
    );
    renderApp('/server?topic=modules');
    const c = await card();
    expect(within(c).getByRole('switch', { name: 'Enable Contributions' })).toBeChecked();
    expect(
      within(c).getByText('On, but hidden from listeners until the Bridge is ready.'),
    ).toBeInTheDocument();
  });

  it('hides the note once the module is active', async () => {
    mockFetch(
      routes([contributions({ enabled: true, active: true, bridge_state: 'ready' })], {
        'GET /admin/settings': { body: settingsWith({ modules: { contributions: true } }) },
      }),
    );
    renderApp('/server?topic=modules');
    const c = await card();
    expect(within(c).getByText('Bridge ready')).toBeInTheDocument();
    expect(within(c).queryByText(/hidden from listeners/)).not.toBeInTheDocument();
  });

  it('turns a module on at once and asks the module list again', async () => {
    const base = settingsWith();
    const calls = mockFetch(
      routes([contributions()], { 'PATCH /admin/settings': patchEcho(base) }),
    );
    renderApp('/server?topic=modules');
    const toggle = within(await card()).getByRole('switch', { name: 'Enable Contributions' });
    await userEvent.setup().click(toggle);
    expect(await screen.findByText('Contributions is on')).toBeInTheDocument();
    expect(calls.find((c) => c.method === 'PATCH')?.body).toEqual({
      modules: { contributions: true },
    });
    expect(toggle).toBeChecked();
    await waitFor(() =>
      expect(calls.filter((c) => c.path === '/admin/modules').length).toBeGreaterThan(1),
    );
  });

  it('locks a module the environment sets, naming the variable', async () => {
    mockFetch(
      routes([contributions({ enabled: true })], {
        'GET /admin/settings': {
          body: settingsWith({
            modules: { contributions: true },
            locked: { 'modules.contributions': 'AUDIOSILO_MODULES_CONTRIBUTIONS_ENABLED' },
          }),
        },
      }),
    );
    renderApp('/server?topic=modules');
    const c = await card();
    expect(within(c).getByRole('switch', { name: 'Enable Contributions' })).toHaveAttribute(
      'aria-disabled',
      'true',
    );
    expect(
      within(c).getByText('Set by AUDIOSILO_MODULES_CONTRIBUTIONS_ENABLED'),
    ).toBeInTheDocument();
  });

  it("names a module the console doesn't know by its id", async () => {
    mockFetch(routes([{ id: 'future_thing', enabled: false, active: false }]));
    renderApp('/server?topic=modules');
    expect(await screen.findByRole('heading', { name: 'future_thing' })).toBeInTheDocument();
    expect(screen.queryByText('Needs a Bridge')).not.toBeInTheDocument();
  });

  it('says so when the server has no modules', async () => {
    mockFetch(routes([]));
    renderApp('/server?topic=modules');
    expect(await screen.findByText('No modules yet')).toBeInTheDocument();
  });

  describe("a module's own page", () => {
    // No module brings a page in Phase 2a: lend Bridge to contributions to try the rule.
    beforeEach(() => {
      MODULE_PAGES.bridge = 'contributions';
    });
    afterEach(() => {
      delete MODULE_PAGES.bridge;
    });

    it('is out of the nav while its module is off, and its link opens General', async () => {
      mockFetch(routes([contributions()]));
      renderApp('/server?topic=bridge');
      const nav = await screen.findByRole('navigation', { name: 'Settings topics' });
      expect(await screen.findByRole('textbox', { name: 'Server name' })).toBeInTheDocument();
      expect(within(nav).queryByRole('link', { name: 'Bridge' })).not.toBeInTheDocument();
      expect(within(nav).getByRole('link', { name: 'Modules' })).toBeInTheDocument();
    });

    it('is in the nav while its module is on', async () => {
      mockFetch(
        routes([contributions({ enabled: true })], {
          'GET /admin/settings': { body: settingsWith({ modules: { contributions: true } }) },
          'GET /admin/bridge': {
            body: {
              paired: false,
              paired_by: null,
              pairing: null,
              online: false,
              kinds: [],
              features: [],
              libraries: [],
              workers: [],
            },
          },
          'GET /admin/bridge/jobs': { body: { jobs: [], next_before: 0 } },
        }),
      );
      renderApp('/server?topic=bridge');
      const nav = await screen.findByRole('navigation', { name: 'Settings topics' });
      expect(await within(nav).findByRole('link', { name: 'Bridge' })).toHaveAttribute(
        'aria-current',
        'page',
      );
    });
  });
});
