import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { InfiniteQueryObserver, QueryClient, type InfiniteData } from '@tanstack/react-query';
import { keys } from '@/api/hooks';
import { setToken } from '@/api/token';
import type {
  AudibleAccount,
  AudiblePlan,
  AudibleTitle,
  AudibleTitlePage,
  BridgeJob,
  BridgeStatus,
} from '@/api/types';
import { mockFetch, type MockReply, type MockRoute } from '@/test/fetch-mock';
import { settingsWith, systemStatus } from '@/test/fixtures';
import { renderApp } from '@/test/render-app';
import { signedInRoutes } from '@/test/routes';
import en from '@/i18n/locales/en.json';
import {
  accountName,
  audibleJobReason,
  backupRunning,
  canReject,
  canSync,
  chosenAccount,
  cleanDest,
  importStatus,
  lookList,
  matchedParam,
  needsLook,
  onServer,
  plannedLine,
  plannedPlaceholder,
  pollTarget,
  reasonKey,
  syncSummary,
  titleByline,
} from './audible-model';
import { storeTitle } from './use-audible';

// Audible (Bridge Phase 4a): Settings > Bridge's read-only Audible accounts card
// and Settings > Audible's titles, both shown only while the module is on.

function account(over: Partial<AudibleAccount> = {}): AudibleAccount {
  return {
    id: 'acct0001',
    label: 'Chris (US)',
    marketplace: 'us',
    user_id: 1,
    user: { id: 1, display_name: 'Chris' },
    library_id: 1,
    library: { id: 1, name: 'Fiction' },
    paused: false,
    policy: 'manual',
    sync: true,
    poll_hours: 6,
    created_at: '2026-11-01T09:00:00Z',
    updated_at: '2026-11-01T09:00:00Z',
    last_poll_at: new Date().toISOString(),
    last_queued_at: null,
    titles: { total: 30, available: 25, preorder: 3, unavailable: 1, gone: 1, matched: 12 },
    template: '',
    policy_since: '2026-11-01T09:00:00Z',
    backup_estimate: { count: 13, bytes: 9_400_000_000 },
    last_sync_at: null,
    refused_at: null,
    ...over,
  };
}

function title(over: Partial<AudibleTitle> = {}): AudibleTitle {
  return {
    account: 'acct0001',
    asin: 'B000000001',
    title: 'The Final Empire',
    authors: ['Brandon Sanderson'],
    series: [{ name: 'Mistborn', position: '1' }],
    release_date: '',
    runtime_s: 36000,
    state: 'available',
    reason: '',
    match: { library_id: 1, path: 'Brandon Sanderson/Mistborn', how: 'asin' },
    rejected: false,
    ignored: false,
    no_sync: false,
    dest_path: '',
    first_seen: '2026-11-01T09:00:00Z',
    updated_at: '2026-11-01T09:00:00Z',
    import_job_id: null,
    import: null,
    needs_attention: false,
    planned: null,
    contested: null,
    can_backup: false,
    ...over,
  };
}

function plan(over: Partial<AudiblePlan> = {}): AudiblePlan {
  return {
    library_id: 1,
    path: 'Ben Aaronovitch/Rivers of London/2 - Moon over Soho/Moon over Soho.m4b',
    file: 'Moon over Soho.m4b',
    kind: 'series_existing',
    siblings: 7,
    state: 'joins',
    conflict: '',
    template: '',
    ...over,
  };
}

const bridgeStatus: BridgeStatus = {
  paired: true,
  paired_at: '2026-10-15T09:00:00Z',
  paired_by: { id: 1, username: 'admin' },
  pairing: null,
  online: true,
  heartbeat_at: new Date().toISOString(),
  version: '0.6.0',
  kinds: ['bridge.verify', 'audible.poll', 'audible.import'],
  features: ['audible', 'bridge'],
  libraries: [],
  workers: [],
  ui_url: 'http://192.168.1.20:8090/',
};

function routes(on: boolean, over: Record<string, MockRoute> = {}) {
  return signedInRoutes({
    'GET /admin/settings': {
      body: settingsWith({ modules: { contributions: false, audible: on } }),
    },
    'GET /admin/bridge': { body: bridgeStatus },
    'GET /admin/bridge/jobs': { body: { jobs: [], next_before: 0 } },
    'GET /admin/audible/accounts': { body: { accounts: [account()] } },
    ...over,
  });
}

beforeEach(() => setToken('stored'));

describe('Audible Bridge requirements', () => {
  const unpaired = { ...bridgeStatus, paired: false, online: false, kinds: [] };

  it('links to pairing when Audible has never had a Bridge', async () => {
    mockFetch(
      routes(true, {
        'GET /admin/bridge': { body: unpaired },
        'GET /admin/audible/accounts': { body: { accounts: [] } },
        'GET /admin/audible/titles': { body: { titles: [], next: '' } },
      }),
    );
    renderApp('/server?topic=audible');
    expect(await screen.findByRole('link', { name: 'Pair a Bridge' })).toHaveAttribute(
      'href',
      '/admin/server?topic=bridge',
    );
    expect(screen.getByText(/Pair a Bridge to check Audible accounts/)).toBeInTheDocument();
  });

  it('disables job starts after unpairing, preserves account actions and refreshes after re-pairing', async () => {
    const calls = mockFetch(
      routes(true, {
        'GET /admin/bridge': { body: unpaired },
        'GET /admin/audible/titles': {
          body: { titles: [title({ match: null, can_backup: true })], next: '' },
        },
      }),
    );
    const { queryClient } = renderApp('/server?topic=audible');
    await screen.findByRole('list', { name: 'Audible titles' });
    for (const name of ['Check now', 'Back up', 'Back up everything...']) {
      const button = screen.getByRole('button', { name });
      expect(button).toBeDisabled();
      expect(button).toHaveAccessibleDescription(/Pair a Bridge/);
    }
    expect(screen.getByRole('button', { name: 'Remove...' })).toBeEnabled();
    expect(screen.getByRole('button', { name: 'Not this book: The Final Empire' })).toBeEnabled();
    expect(calls.some((c) => c.method === 'POST' && /poll|backup/.test(c.path))).toBe(false);
    act(() => queryClient.setQueryData(keys.bridge, bridgeStatus));
    await waitFor(() => expect(screen.getByRole('button', { name: 'Check now' })).toBeEnabled());
    expect(screen.getByRole('button', { name: 'Back up' })).toBeEnabled();
    expect(screen.queryByRole('link', { name: 'Pair a Bridge' })).not.toBeInTheDocument();
  });

  it.each([
    [{ ...bridgeStatus, online: false }, true, true, /Supported jobs can be queued/],
    [{ ...bridgeStatus, kinds: ['audible.poll'] }, true, false, /unsupported actions are disabled/],
    [
      { ...bridgeStatus, kinds: ['audible.import'] },
      false,
      true,
      /unsupported actions are disabled/,
    ],
  ] as const)(
    'distinguishes offline from unsupported jobs: %o',
    async (status, poll, backup, note) => {
      mockFetch(
        routes(true, {
          'GET /admin/bridge': { body: status },
          'GET /admin/audible/titles': {
            body: { titles: [title({ match: null, can_backup: true })], next: '' },
          },
        }),
      );
      renderApp('/server?topic=audible');
      await screen.findByRole('list', { name: 'Audible titles' });
      expect(screen.getByText(note)).toBeInTheDocument();
      expect(screen.getByRole('button', { name: 'Check now' }).hasAttribute('disabled')).toBe(
        !poll,
      );
      expect(screen.getByRole('button', { name: 'Back up' }).hasAttribute('disabled')).toBe(
        !backup,
      );
    },
  );
});

/** Runs fn with the process in time zone tz (Node reads a new TZ at once). */
async function inZone<T>(tz: string, fn: () => Promise<T>): Promise<T> {
  const zone = process.env.TZ;
  process.env.TZ = tz;
  try {
    return await fn();
  } finally {
    if (zone === undefined) delete process.env.TZ;
    else process.env.TZ = zone;
  }
}

describe('audible model', () => {
  it('words the filters and names', () => {
    expect(matchedParam('matched')).toBe(1);
    expect(matchedParam('missing')).toBe(0);
    expect(matchedParam('')).toBeUndefined();
    expect(matchedParam('look')).toBe(0);
    const a = account();
    const b = account({ id: 'acct0002' });
    expect(pollTarget([a], '')).toBe(a);
    expect(pollTarget([a, b], '')).toBeUndefined();
    expect(pollTarget([a, b], 'acct0002')).toBe(b);
    expect(chosenAccount([a, b], 'acct0002')).toBe('acct0002');
    expect(chosenAccount([a], 'acct0002')).toBe('');
    expect(chosenAccount([a, b], '')).toBe('');
    expect(titleByline(title())).toBe('Brandon Sanderson · Mistborn 1');
    expect(titleByline(title({ authors: [], series: [] }))).toBe('');
    const t = ((k: string, o?: Record<string, string>) => `${k}:${o?.name}:${o?.store}`) as never;
    expect(accountName(t, account({ label: '' }))).toBe('audible.account.unnamed:Chris:US');
  });
});

describe('audible accounts kept without a Bridge or the module (server#294)', () => {
  const unpaired = { ...bridgeStatus, paired: false, online: false, kinds: [] };

  it('lists them on Settings > Bridge while no Bridge is paired, with Remove', async () => {
    mockFetch(routes(true, { 'GET /admin/bridge': { body: unpaired } }));
    renderApp('/server?topic=bridge');
    const heading = await screen.findByRole('heading', { name: 'Audible accounts' });
    const card = heading.closest('section') as HTMLElement;
    expect(await within(card).findByText('Chris (US)')).toBeInTheDocument();
    expect(within(card).getByRole('button', { name: 'Remove...' })).toBeEnabled();
  });

  it('reads them once while unpaired: nothing but Remove changes them', async () => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
    try {
      const calls = mockFetch(routes(true, { 'GET /admin/bridge': { body: unpaired } }));
      renderApp('/server?topic=bridge');
      await screen.findByRole('heading', { name: 'Audible accounts' });
      const reads = () => calls.filter((c) => c.path === '/admin/audible/accounts').length;
      const before = reads();
      await act(() => vi.advanceTimersByTimeAsync(90_000));
      expect(reads()).toBe(before);
    } finally {
      vi.useRealTimers();
    }
  });

  it('leaves the card out while unpaired with no accounts', async () => {
    const calls = mockFetch(
      routes(true, {
        'GET /admin/bridge': { body: unpaired },
        'GET /admin/audible/accounts': { body: { accounts: [] } },
      }),
    );
    renderApp('/server?topic=bridge');
    await waitFor(() => expect(calls.some((c) => c.path === '/admin/audible/accounts')).toBe(true));
    expect(screen.queryByRole('heading', { name: 'Audible accounts' })).not.toBeInTheDocument();
  });

  it('leaves the card out while unpaired with the module off', async () => {
    const calls = mockFetch(routes(false, { 'GET /admin/bridge': { body: unpaired } }));
    renderApp('/server?topic=bridge');
    expect(await screen.findByRole('button', { name: 'Connect a Bridge' })).toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: 'Audible accounts' })).not.toBeInTheDocument();
    expect(calls.some((c) => c.path.startsWith('/admin/audible'))).toBe(false);
  });

  it('lists them on Settings > Modules while the module is off, and removes one', async () => {
    let removed = false;
    const calls = mockFetch(
      routes(false, {
        'GET /admin/system': { body: systemStatus() },
        'GET /admin/modules': {
          body: {
            modules: [
              {
                id: 'audible',
                enabled: false,
                active: false,
                requires_bridge: 'audible',
                bridge_state: 'offline',
              },
            ],
          },
        },
        'GET /admin/audible/accounts': () => ({
          body: { accounts: removed ? [] : [account()] },
        }),
        'DELETE /admin/audible/accounts/acct0001': () => {
          removed = true;
          return { status: 204 };
        },
      }),
    );
    renderApp('/server?topic=modules');
    const kept = await screen.findByRole('heading', { name: "Accounts kept while it's off" });
    const section = kept.closest('section') as HTMLElement;
    expect(within(section).getByText('Chris (US)')).toBeInTheDocument();
    const user = userEvent.setup();
    await user.click(within(section).getByRole('button', { name: 'Remove...' }));
    const dialog = await screen.findByRole('dialog', { name: 'Remove Chris (US)?' });
    await user.click(within(dialog).getByRole('button', { name: 'Remove' }));
    await waitFor(() =>
      expect(
        calls.some((c) => c.method === 'DELETE' && c.path === '/admin/audible/accounts/acct0001'),
      ).toBe(true),
    );
    await waitFor(() =>
      expect(
        screen.queryByRole('heading', { name: "Accounts kept while it's off" }),
      ).not.toBeInTheDocument(),
    );
  });

  it("says on the Bridge's Audible setup step that the module is off, with the way to Modules", async () => {
    mockFetch(
      routes(false, {
        'GET /admin/bridge': {
          body: {
            ...bridgeStatus,
            setup: {
              items: [
                { id: 'paired', done: true, required: true },
                { id: 'audible', done: true, required: false },
              ],
              can_contribute: false,
            },
          },
        },
      }),
    );
    const { router } = renderApp('/server?topic=bridge');
    expect(
      await screen.findByText("The Audible module is off, so the accounts aren't checked."),
    ).toBeInTheDocument();
    await userEvent.setup().click(screen.getByRole('link', { name: 'Open Modules' }));
    await waitFor(() => expect(router.state.location.search).toEqual({ topic: 'modules' }));
  });

  it('says nothing of the module on the setup step while it is on', async () => {
    mockFetch(
      routes(true, {
        'GET /admin/bridge': {
          body: {
            ...bridgeStatus,
            setup: {
              items: [{ id: 'audible', done: true, required: false }],
              can_contribute: false,
            },
          },
        },
      }),
    );
    renderApp('/server?topic=bridge');
    expect(await screen.findByText('Audible signed in')).toBeInTheDocument();
    expect(screen.queryByText(/The Audible module is off/)).not.toBeInTheDocument();
  });

  it('lists nothing on Settings > Modules while the module is on', async () => {
    const calls = mockFetch(
      routes(true, {
        'GET /admin/system': { body: systemStatus() },
        'GET /admin/modules': {
          body: {
            modules: [
              {
                id: 'audible',
                enabled: true,
                active: false,
                requires_bridge: 'audible',
                bridge_state: 'offline',
              },
            ],
          },
        },
      }),
    );
    renderApp('/server?topic=modules');
    expect(await screen.findByRole('heading', { name: 'Audible' })).toBeInTheDocument();
    expect(
      screen.queryByRole('heading', { name: "Accounts kept while it's off" }),
    ).not.toBeInTheDocument();
    expect(calls.some((c) => c.path === '/admin/audible/accounts')).toBe(false);
  });
});

describe('settings > bridge > audible accounts', () => {
  it('is hidden while the module is off, and asks for nothing', async () => {
    const calls = mockFetch(routes(false));
    renderApp('/server?topic=bridge');
    expect(await screen.findByRole('heading', { name: 'Libraries' })).toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: 'Audible accounts' })).not.toBeInTheDocument();
    expect(calls.some((c) => c.path.startsWith('/admin/audible'))).toBe(false);
    const nav = screen.getByRole('navigation', { name: 'Settings topics' });
    expect(within(nav).queryByRole('link', { name: 'Audible' })).not.toBeInTheDocument();
  });

  it('shows each account read-only while the module is on', async () => {
    mockFetch(
      routes(true, {
        'GET /admin/audible/accounts': {
          body: {
            accounts: [
              account(),
              account({
                id: 'acct0002',
                label: 'Sam (UK)',
                marketplace: 'uk',
                library_id: null,
                library: null,
                paused: true,
                titles: {
                  total: 1,
                  available: 1,
                  preorder: 0,
                  unavailable: 0,
                  gone: 0,
                  matched: 0,
                },
              }),
            ],
          },
        },
      }),
    );
    renderApp('/server?topic=bridge');
    const heading = await screen.findByRole('heading', { name: 'Audible accounts' });
    const card = heading.closest('section') as HTMLElement;
    const list = await within(card).findByRole('list', { name: 'Audible accounts' });
    const rows = within(list).getAllByRole('listitem');
    expect(rows).toHaveLength(2);
    expect(within(rows[0]).getByText('Chris (US)')).toBeInTheDocument();
    expect(within(rows[0]).getByText('Fiction')).toBeInTheDocument();
    expect(within(rows[0]).getByText('30 titles, 12 on this server')).toBeInTheDocument();
    expect(within(rows[0]).getByText('Only books you choose')).toBeInTheDocument();
    // The store as its domain, and a check under a minute ago as "just now".
    expect(within(rows[0]).getByText('audible.com')).toBeInTheDocument();
    expect(within(rows[1]).getByText('audible.co.uk')).toBeInTheDocument();
    expect(within(rows[0]).getByText('Checked just now')).toBeInTheDocument();
    expect(within(rows[1]).getByText('Paused')).toBeInTheDocument();
    expect(within(rows[1]).getByText('1 title, 0 on this server')).toBeInTheDocument();
    expect(within(rows[1]).getByRole('button', { name: 'Check now' })).toBeDisabled();
    // Nothing on the card edits the mapping: that is the Bridge's page.
    expect(within(card).queryByRole('textbox')).not.toBeInTheDocument();
    expect(within(card).getByRole('link', { name: 'View titles' })).toHaveAttribute(
      'href',
      '/admin/server?topic=audible',
    );
  });

  it("links to the Bridge's Accounts tab when there are none", async () => {
    mockFetch(routes(true, { 'GET /admin/audible/accounts': { body: { accounts: [] } } }));
    renderApp('/server?topic=bridge');
    expect(await screen.findByText('No Audible accounts yet')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: /Open the Bridge's Accounts tab/ })).toHaveAttribute(
      'href',
      'http://192.168.1.20:8090/#/accounts',
    );
  });

  it('checks an account now and says so', async () => {
    const calls = mockFetch(
      routes(true, {
        'POST /admin/audible/accounts/acct0001/poll': {
          status: 201,
          body: { id: 9, kind: 'audible.poll' } as Partial<BridgeJob>,
        },
      }),
    );
    renderApp('/server?topic=bridge');
    const heading = await screen.findByRole('heading', { name: 'Audible accounts' });
    const card = heading.closest('section') as HTMLElement;
    await userEvent.setup().click(await within(card).findByRole('button', { name: 'Check now' }));
    expect(await screen.findByText('Checking Chris (US)')).toBeInTheDocument();
    expect(calls.some((c) => c.method === 'POST' && c.path.endsWith('/poll'))).toBe(true);
  });

  it('removes an account from this server after saying the Bridge still holds its sign-in', async () => {
    let removed = false;
    const calls = mockFetch(
      routes(true, {
        'GET /admin/audible/accounts': () => ({
          body: { accounts: removed ? [] : [account()] },
        }),
        'DELETE /admin/audible/accounts/acct0001': () => {
          removed = true;
          return { status: 204 };
        },
      }),
    );
    renderApp('/server?topic=bridge');
    const heading = await screen.findByRole('heading', { name: 'Audible accounts' });
    const card = heading.closest('section') as HTMLElement;
    const user = userEvent.setup();
    await user.click(await within(card).findByRole('button', { name: 'Remove...' }));
    const dialog = await screen.findByRole('dialog', { name: 'Remove Chris (US)?' });
    expect(
      within(dialog).getByText('This removes the account and its titles from this server only.'),
    ).toBeInTheDocument();
    expect(
      within(dialog).getByText(
        /To sign the device out of Amazon, remove it on the Bridge's Accounts page too/,
      ),
    ).toBeInTheDocument();
    // What else goes: open checks, backups and syncs (bridge#47), and the
    // Bridge's mapping that would fail until mapped again (server#294).
    expect(
      within(dialog).getByText(/any check, backup or sync that hasn't finished is cancelled/),
    ).toBeInTheDocument();
    expect(within(dialog).getByText(/its checks fail until you map it again/)).toBeInTheDocument();
    expect(calls.some((c) => c.method === 'DELETE')).toBe(false);
    await user.click(within(dialog).getByRole('button', { name: 'Remove' }));
    await waitFor(() =>
      expect(
        calls.some((c) => c.method === 'DELETE' && c.path === '/admin/audible/accounts/acct0001'),
      ).toBe(true),
    );
    expect(await screen.findByText('Chris (US) removed')).toBeInTheDocument();
    expect(await within(card).findByText('No Audible accounts yet')).toBeInTheDocument();
  });

  it('says a poll being cancelled is still stopping, and words an account gone since', async () => {
    let reply: MockReply = {
      status: 200,
      body: { id: 9, kind: 'audible.poll', cancel_requested: true } as Partial<BridgeJob>,
    };
    mockFetch(routes(true, { 'POST /admin/audible/accounts/acct0001/poll': () => reply }));
    renderApp('/server?topic=bridge');
    const heading = await screen.findByRole('heading', { name: 'Audible accounts' });
    const card = heading.closest('section') as HTMLElement;
    const user = userEvent.setup();
    await user.click(await within(card).findByRole('button', { name: 'Check now' }));
    expect(
      await screen.findByText('The check of Chris (US) is still stopping'),
    ).toBeInTheDocument();
    reply = {
      status: 404,
      body: { error: 'no such Audible account', code: 'unknown_account' },
    };
    await user.click(within(card).getByRole('button', { name: 'Check now' }));
    expect(
      await screen.findByText(/That Audible account isn't on this server any more/),
    ).toBeInTheDocument();
  });
});

describe('settings > audible', () => {
  it('lists the titles, which are here, and filters them', async () => {
    const calls = mockFetch(
      routes(true, {
        'GET /admin/audible/titles': (req) => ({
          body: {
            titles:
              req.query.get('matched') === '0'
                ? [title({ asin: 'B000000002', title: 'The Well of Ascension', match: null })]
                : [
                    title(),
                    title({
                      asin: 'B000000003',
                      title: 'The Hero of Ages',
                      state: 'preorder',
                      release_date: '2026-11-20',
                      match: null,
                    }),
                  ],
            next: '',
          },
        }),
      }),
    );
    // A preorder's release day as Audible gives it, wherever the console is
    // (west of UTC, the day read as a moment would show the 19th).
    const rows = await inZone('Pacific/Pago_Pago', async () => {
      renderApp('/server?topic=audible');
      const list = await screen.findByRole('list', { name: 'Audible titles' });
      const rows = within(list).getAllByRole('listitem');
      expect(within(rows[1]).getByText(/Releases Nov 20, 2026/)).toBeInTheDocument();
      return rows;
    });
    expect(rows).toHaveLength(2);
    expect(within(rows[0]).getByRole('link', { name: 'On this server' })).toHaveAttribute(
      'href',
      expect.stringContaining('/admin/library/book?library=1&path=Brandon'),
    );
    expect(within(rows[1]).getByText('Preorder')).toBeInTheDocument();
    expect(within(rows[1]).getByText('Not here')).toBeInTheDocument();
    const user = userEvent.setup();
    await user.selectOptions(screen.getByRole('combobox', { name: 'On this server' }), 'missing');
    expect(await screen.findByText('The Well of Ascension')).toBeInTheDocument();
    await waitFor(() =>
      expect(
        calls.some((c) => c.path === '/admin/audible/titles' && c.query.get('matched') === '0'),
      ).toBe(true),
    );
    await user.selectOptions(screen.getByRole('combobox', { name: 'State' }), 'gone');
    await waitFor(() => expect(calls.some((c) => c.query.get('state') === 'gone')).toBe(true));
    // One account: Check now polls it and Remove is offered, no account picker.
    expect(screen.queryByRole('combobox', { name: 'Account' })).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Check now' })).toBeEnabled();
    expect(screen.getByRole('button', { name: 'Remove...' })).toBeInTheDocument();
  });

  it('lists the titles whose planned backup needs a look, preorders first', async () => {
    let askedNotHere = 0;
    mockFetch(
      routes(true, {
        'GET /admin/audible/titles': (req) => {
          const notHere = req.query.get('matched') === '0';
          if (notHere) askedNotHere++;
          return {
            body: {
              titles: notHere
                ? [
                    title({
                      asin: 'B000000002',
                      title: 'Quiet One',
                      match: null,
                      planned: plan({ kind: 'standalone', siblings: 0 }),
                    }),
                    title({
                      asin: 'B000000003',
                      title: 'New Author',
                      match: null,
                      planned: plan({ kind: 'standalone', siblings: 0, state: 'new' }),
                    }),
                    title({
                      asin: 'B000000004',
                      title: 'Coming Soon',
                      state: 'preorder',
                      release_date: '2026-12-20',
                      match: null,
                      planned: plan({ kind: 'series_new', siblings: 0 }),
                    }),
                  ]
                : [title()],
              next: '',
            },
          };
        },
      }),
    );
    renderApp('/server?topic=audible');
    await screen.findByRole('list', { name: 'Audible titles' });
    const user = userEvent.setup();
    const onServer = screen.getByRole('combobox', { name: 'On this server' });
    await user.selectOptions(onServer, 'look');
    await waitFor(() => expect(screen.queryByText('Quiet One')).not.toBeInTheDocument());
    const list = await screen.findByRole('list', { name: 'Audible titles' });
    const rows = within(list).getAllByRole('listitem');
    expect(rows).toHaveLength(2);
    expect(within(rows[0]).getByText('Coming Soon')).toBeInTheDocument();
    expect(within(rows[0]).getByText(/the first book of its series here/)).toBeInTheDocument();
    expect(within(rows[1]).getByText('New Author')).toBeInTheDocument();
    expect(within(rows[1]).getByText(/a new author folder/)).toBeInTheDocument();
    // "Not here" asks the server the same thing, so it shows the pages already
    // read rather than asking again.
    await user.selectOptions(onServer, 'missing');
    expect(await screen.findByText('Quiet One')).toBeInTheDocument();
    expect(askedNotHere).toBe(1);
  });

  it('says so when there are none, and when a filter matches nothing', async () => {
    mockFetch(routes(true, { 'GET /admin/audible/titles': { body: { titles: [], next: '' } } }));
    renderApp('/server?topic=audible');
    expect(await screen.findByText('No titles yet')).toBeInTheDocument();
    await userEvent.setup().type(screen.getByRole('textbox', { name: 'Search titles' }), 'zzz');
    expect(await screen.findByText('No titles match')).toBeInTheDocument();
  });

  it('drops the account filter once that account is gone', async () => {
    let accounts = [account(), account({ id: 'acct0002', label: 'Sam (UK)' })];
    mockFetch(
      routes(true, {
        'GET /admin/audible/accounts': () => ({ body: { accounts } }),
        'GET /admin/audible/titles': (req) => ({
          body: { titles: req.query.get('account') === 'acct0002' ? [] : [title()], next: '' },
        }),
      }),
    );
    const { queryClient } = renderApp('/server?topic=audible');
    await userEvent
      .setup()
      .selectOptions(await screen.findByRole('combobox', { name: 'Account' }), 'acct0002');
    expect(await screen.findByText('No titles match')).toBeInTheDocument();
    // Sam's account is removed on the Bridge: the accounts, asked again, drop it.
    accounts = [account()];
    await queryClient.invalidateQueries({ queryKey: keys.audibleAccounts });
    expect(await screen.findByText('The Final Empire')).toBeInTheDocument();
    expect(screen.queryByRole('combobox', { name: 'Account' })).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Check now' })).toBeEnabled();
  });

  it('refreshes the titles once a poll lands', async () => {
    let polled = '2026-11-01T09:00:00Z';
    let titles = [title()];
    mockFetch(
      routes(true, {
        'GET /admin/audible/accounts': () => ({
          body: { accounts: [account({ last_poll_at: polled })] },
        }),
        'GET /admin/audible/titles': () => ({ body: { titles, next: '' } }),
      }),
    );
    const { queryClient } = renderApp('/server?topic=audible');
    expect(await screen.findByText('The Final Empire')).toBeInTheDocument();
    // The poll lands: the accounts, asked again every 30 s, carry its time.
    polled = '2026-11-01T10:00:00Z';
    titles = [title(), title({ asin: 'B000000005', title: 'Elantris', match: null })];
    await queryClient.invalidateQueries({ queryKey: keys.audibleAccounts });
    expect(await screen.findByText('Elantris')).toBeInTheDocument();
  });

  it('opens General while the module is off', async () => {
    mockFetch(routes(false));
    renderApp('/server?topic=audible');
    expect(await screen.findByRole('textbox', { name: 'Server name' })).toBeInTheDocument();
  });
});

describe('audible backups', () => {
  it('offers Not this book except on a backup of its own, and reads a rejected match as not here', () => {
    expect(canReject(title())).toBe(true);
    expect(canReject(title({ match: null }))).toBe(true);
    const own = { library_id: 1, path: 'Brandon Sanderson/Mistborn', how: 'import' } as const;
    expect(canReject(title({ match: own }))).toBe(false);
    expect(canReject(title({ match: own, rejected: true }))).toBe(true);
    expect(onServer(title())).toBe(true);
    expect(onServer(title({ rejected: true }))).toBe(false);
    expect(onServer(title({ match: null }))).toBe(false);
    expect(cleanDest('  /A/B/C.m4b/ ')).toBe('A/B/C.m4b');
  });

  it('asks the titles again only while a backup job is queued or running', () => {
    for (const state of ['queued', 'claimed', 'running'] as const) {
      expect(backupRunning(title({ import: { job_id: 4, state, code: '' } }))).toBe(true);
    }
    for (const state of ['pending', 'needs_attention', 'done', 'failed', 'cancelled'] as const) {
      expect(backupRunning(title({ import: { job_id: 4, state, code: '' } }))).toBe(false);
    }
    expect(backupRunning(title())).toBe(false);
  });

  it("words a title's backup and its code", () => {
    expect(importStatus(title())).toBeNull();
    expect(importStatus(title({ import: { job_id: null, state: 'pending', code: '' } }))?.key).toBe(
      'audible.import.pending',
    );
    expect(
      importStatus(title({ import: { job_id: 4, state: 'failed', code: 'disk_full' } })),
    ).toEqual({ key: 'audible.import.failed', tone: 'bad', code: 'audible.code.disk_full' });
    // A code this console doesn't know reads as the state alone.
    expect(
      importStatus(title({ import: { job_id: 4, state: 'failed', code: 'something_new' } }))?.code,
    ).toBeUndefined();
    expect(
      importStatus(
        title({ needs_attention: true, import: { job_id: null, state: 'pending', code: '' } }),
      )?.key,
    ).toBe('audible.import.attention');
  });

  it("words the Bridge's newer Audible failure codes (drm_free, a check's refusals)", () => {
    const strings = en as Record<string, string>;
    expect(
      importStatus(title({ import: { job_id: 4, state: 'failed', code: 'drm_free' } }))?.code,
    ).toBe('audible.code.drm_free');
    const job = (kind: string, code: unknown) => ({ kind, result: { code } });
    expect(audibleJobReason(job('audible.import', 'drm_free'))).toBe('audible.code.drm_free');
    expect(audibleJobReason(job('audible.poll', 'audible_refused'))).toBe(
      'audible.pollCode.audible_refused',
    );
    expect(audibleJobReason(job('audible.poll', 'audible_sign_in'))).toBe(
      'audible.pollCode.audible_sign_in',
    );
    for (const key of [
      'audible.code.drm_free',
      'audible.pollCode.audible_refused',
      'audible.pollCode.audible_sign_in',
    ]) {
      expect(strings[key]).toBeTruthy();
    }
    // Unknown codes, other kinds, and no result keep the Bridge's own text.
    expect(audibleJobReason(job('audible.poll', 'something_new'))).toBeUndefined();
    expect(audibleJobReason(job('audible.poll', 'drm_free'))).toBeUndefined();
    expect(audibleJobReason(job('sidecars.generate', 'audible_refused'))).toBeUndefined();
    expect(audibleJobReason({ kind: 'audible.poll', result: null })).toBeUndefined();
    // A place sync names the same refusals; a backup's code may be its error's prefix.
    expect(audibleJobReason(job('audible.sync', 'audible_sign_in'))).toBe(
      'audible.pollCode.audible_sign_in',
    );
    expect(
      audibleJobReason({ kind: 'audible.import', result: {}, error: 'drm_free: only an MP3' }),
    ).toBe('audible.code.drm_free');
    expect(
      audibleJobReason({ kind: 'audible.import', result: {}, error: 'something else' }),
    ).toBeUndefined();
    // An exists the Bridge names the conflict of reads as the title's row does.
    expect(
      audibleJobReason({ kind: 'audible.import', result: { code: 'exists', conflict: 'book' } }),
    ).toBe('audible.code.exists.book');
    expect(audibleJobReason({ kind: 'audible.import', result: { code: 'exists' } })).toBe(
      'audible.code.exists',
    );
  });

  it('backs a title up, and says a preorder waits for its release', async () => {
    let pending = false;
    const calls = mockFetch(
      routes(true, {
        'GET /admin/audible/titles': {
          body: {
            titles: [
              title({
                asin: 'B000000002',
                title: 'The Well of Ascension',
                match: null,
                can_backup: true,
              }),
              title({
                asin: 'B000000004',
                title: 'The Bands of Mourning',
                match: null,
                import: { job_id: 7, state: 'failed', code: 'exists' },
              }),
            ],
            next: '',
          },
        },
        'POST /admin/audible/titles/acct0001/B000000002/backup': () =>
          pending
            ? { status: 200, body: { pending: true, job_id: null } }
            : { status: 202, body: { pending: false, job_id: 12 } },
      }),
    );
    renderApp('/server?topic=audible');
    const list = await screen.findByRole('list', { name: 'Audible titles' });
    const rows = within(list).getAllByRole('listitem');
    expect(within(rows[1]).getByText('Backup failed')).toBeInTheDocument();
    expect(
      within(rows[1]).getByText(/A different file is already where it would go/),
    ).toBeInTheDocument();
    const user = userEvent.setup();
    await user.click(within(rows[0]).getByRole('button', { name: 'Back up' }));
    expect(await screen.findByText('Backing up The Well of Ascension')).toBeInTheDocument();
    expect(
      calls.some(
        (c) => c.method === 'POST' && c.path === '/admin/audible/titles/acct0001/B000000002/backup',
      ),
    ).toBe(true);
    pending = true;
    await user.click(within(rows[0]).getByRole('button', { name: 'Back up' }));
    expect(
      await screen.findByText('The Well of Ascension is backed up on release'),
    ).toBeInTheDocument();
    expect(screen.getByText(/A personal backup of books you bought/)).toBeInTheDocument();
  });

  it("keeps a paused account's titles from Back up, as Back up everything", async () => {
    mockFetch(
      routes(true, {
        'GET /admin/audible/accounts': { body: { accounts: [account({ paused: true })] } },
        'GET /admin/audible/titles': {
          body: { titles: [title({ match: null, can_backup: true })], next: '' },
        },
      }),
    );
    renderApp('/server?topic=audible');
    const list = await screen.findByRole('list', { name: 'Audible titles' });
    expect(within(list).getByRole('button', { name: 'Back up' })).toBeDisabled();
  });

  it('marks a title Not this book and sets where it goes', async () => {
    const calls = mockFetch(
      routes(true, {
        'GET /admin/audible/titles': {
          body: { titles: [title({ match: null, can_backup: true })], next: '' },
        },
        'PATCH /admin/audible/titles/acct0001/B000000001': (req) => ({
          body: title({ match: null, can_backup: true, ...(req.body as object) }),
        }),
      }),
    );
    renderApp('/server?topic=audible');
    const user = userEvent.setup();
    await user.click(
      await screen.findByRole('button', { name: 'Not this book: The Final Empire' }),
    );
    await waitFor(() =>
      expect(
        calls.some((c) => c.method === 'PATCH' && (c.body as { rejected?: boolean }).rejected),
      ).toBe(true),
    );
    await user.click(screen.getByRole('button', { name: 'Destination for The Final Empire' }));
    const dialog = await screen.findByRole('dialog', { name: 'Where The Final Empire goes' });
    await user.type(within(dialog).getByRole('textbox'), ' /Sanderson/Final Empire.m4b/');
    await user.click(within(dialog).getByRole('button', { name: 'Save' }));
    await waitFor(() =>
      expect(
        calls.some(
          (c) =>
            c.method === 'PATCH' &&
            (c.body as { dest_path?: string }).dest_path === 'Sanderson/Final Empire.m4b',
        ),
      ).toBe(true),
    );
  });

  it("says when a title's own destination is no longer valid", async () => {
    mockFetch(
      routes(true, {
        'GET /admin/audible/titles': {
          body: {
            titles: [
              title({
                match: null,
                can_backup: true,
                dest_path: '.hidden/A.m4b',
                dest_invalid: true,
              }),
              title({
                asin: 'B000000002',
                title: 'Well of Ascension',
                match: null,
                can_backup: true,
                dest_path: 'Sanderson/Well.m4b',
              }),
            ],
            next: '',
          },
        },
      }),
    );
    renderApp('/server?topic=audible');
    expect(
      await screen.findByText(
        'Your destination is no longer valid, so its backup would be refused. Set it again with Destination.',
      ),
    ).toBeInTheDocument();
    expect(screen.getAllByText('Your destination is checked when the backup starts.')).toHaveLength(
      1,
    );
  });

  it('backs everything up after saying how many and how much', async () => {
    const calls = mockFetch(
      routes(true, {
        'POST /admin/audible/accounts/acct0001/backup-all': { body: { queued: 13 } },
      }),
    );
    renderApp('/server?topic=bridge');
    const heading = await screen.findByRole('heading', { name: 'Audible accounts' });
    const card = heading.closest('section') as HTMLElement;
    const user = userEvent.setup();
    await user.click(await within(card).findByRole('button', { name: 'Back up everything...' }));
    const dialog = await screen.findByRole('dialog', {
      name: 'Back up everything from Chris (US)?',
    });
    expect(within(dialog).getByText('13 books, ~9.4 GB.')).toBeInTheDocument();
    expect(within(dialog).getByText(/check your local law/)).toBeInTheDocument();
    expect(calls.some((c) => c.path.endsWith('/backup-all'))).toBe(false);
    await user.click(within(dialog).getByRole('button', { name: 'Back up 13 books' }));
    expect(await screen.findByText('13 backups queued for Chris (US)')).toBeInTheDocument();
    expect(calls.some((c) => c.method === 'POST' && c.path.endsWith('/backup-all'))).toBe(true);
  });

  it('says how many titles everything skipped for a destination no longer valid', async () => {
    mockFetch(
      routes(true, {
        'POST /admin/audible/accounts/acct0001/backup-all': { body: { queued: 11, skipped: 2 } },
      }),
    );
    renderApp('/server?topic=bridge');
    const heading = await screen.findByRole('heading', { name: 'Audible accounts' });
    const card = heading.closest('section') as HTMLElement;
    const user = userEvent.setup();
    await user.click(await within(card).findByRole('button', { name: 'Back up everything...' }));
    const dialog = await screen.findByRole('dialog', {
      name: 'Back up everything from Chris (US)?',
    });
    await user.click(within(dialog).getByRole('button', { name: 'Back up 13 books' }));
    expect(await screen.findByText('11 backups queued for Chris (US)')).toBeInTheDocument();
    expect(
      screen.getByText(
        '2 titles skipped: their destination is no longer valid. Set it again with Destination...',
      ),
    ).toBeInTheDocument();
  });

  it("points to Account settings when the account's stored template no longer works", async () => {
    mockFetch(
      routes(true, {
        'POST /admin/audible/accounts/acct0001/backup-all': {
          status: 400,
          body: {
            error: 'invalid: template: invalid template: unknown variable {Bogus}',
            code: 'invalid_template',
            reason: 'account',
          },
        },
      }),
    );
    renderApp('/server?topic=bridge');
    const heading = await screen.findByRole('heading', { name: 'Audible accounts' });
    const card = heading.closest('section') as HTMLElement;
    const user = userEvent.setup();
    await user.click(await within(card).findByRole('button', { name: 'Back up everything...' }));
    const dialog = await screen.findByRole('dialog', {
      name: 'Back up everything from Chris (US)?',
    });
    await user.click(within(dialog).getByRole('button', { name: 'Back up 13 books' }));
    expect(
      await within(dialog).findByText(
        "This account's placement template no longer works. Fix it in Account settings and back up again.",
      ),
    ).toBeInTheDocument();
    expect(within(dialog).queryByText(/unknown variable/)).toBeNull();
  });

  it('is off with nothing to back up or a paused account', async () => {
    mockFetch(
      routes(true, {
        'GET /admin/audible/accounts': {
          body: {
            accounts: [
              account({ backup_estimate: { count: 0, bytes: 0 } }),
              account({ id: 'acct0002', label: 'Paused', paused: true }),
            ],
          },
        },
      }),
    );
    renderApp('/server?topic=bridge');
    const heading = await screen.findByRole('heading', { name: 'Audible accounts' });
    const card = heading.closest('section') as HTMLElement;
    const buttons = await within(card).findAllByRole('button', { name: 'Back up everything...' });
    expect(buttons).toHaveLength(2);
    for (const b of buttons) expect(b).toBeDisabled();
  });

  it("updates a title's row from the server's answer, without reading every page again", async () => {
    const one = title({ match: null, can_backup: true });
    const calls = mockFetch(
      routes(true, {
        'GET /admin/audible/titles': { body: { titles: [one], next: '' } },
        'POST /admin/audible/titles/acct0001/B000000001/backup': {
          status: 202,
          body: {
            pending: false,
            job_id: 12,
            title: { ...one, can_backup: false, import: { job_id: 12, state: 'queued', code: '' } },
          },
        },
      }),
    );
    renderApp('/server?topic=audible');
    const list = await screen.findByRole('list', { name: 'Audible titles' });
    const reads = () => calls.filter((c) => c.path === '/admin/audible/titles').length;
    const before = reads();
    const user = userEvent.setup();
    await user.click(within(list).getByRole('button', { name: 'Back up' }));
    expect(await within(list).findByText('Backup waiting')).toBeInTheDocument();
    expect(within(list).queryByRole('button', { name: 'Back up' })).not.toBeInTheDocument();
    expect(reads()).toBe(before);
  });

  it('keeps each row busy on its own while its Back up runs', async () => {
    const releases: Record<string, () => void> = {};
    const hold = (asin: string) => () =>
      new Promise<MockReply>((resolve) => {
        releases[asin] = () => resolve({ status: 202, body: { pending: false, job_id: 12 } });
      });
    mockFetch(
      routes(true, {
        'GET /admin/audible/titles': {
          body: {
            titles: [
              title({ match: null, can_backup: true }),
              title({
                asin: 'B000000002',
                title: 'The Well of Ascension',
                match: null,
                can_backup: true,
              }),
            ],
            next: '',
          },
        },
        'POST /admin/audible/titles/acct0001/B000000001/backup': hold('B000000001'),
        'POST /admin/audible/titles/acct0001/B000000002/backup': hold('B000000002'),
      }),
    );
    renderApp('/server?topic=audible');
    const list = await screen.findByRole('list', { name: 'Audible titles' });
    const rows = within(list).getAllByRole('listitem');
    const backUp = (row: HTMLElement) => within(row).getByRole('button', { name: 'Back up' });
    const user = userEvent.setup();
    await user.click(backUp(rows[0]));
    await user.click(backUp(rows[1]));
    await waitFor(() => expect(backUp(rows[1])).toBeDisabled());
    // The first is still running while the second started.
    expect(backUp(rows[0])).toBeDisabled();
    act(() => releases.B000000001());
    await waitFor(() => expect(backUp(rows[0])).toBeEnabled());
    expect(backUp(rows[1])).toBeDisabled();
    act(() => releases.B000000002());
    await waitFor(() => expect(backUp(rows[1])).toBeEnabled());
  });

  it('shows a title marked Not this book as not here, and offers no Not this book on its own backup', async () => {
    mockFetch(
      routes(true, {
        'GET /admin/audible/titles': {
          body: {
            titles: [
              title({ rejected: true, can_backup: true }),
              title({
                asin: 'B000000002',
                title: 'The Well of Ascension',
                match: { library_id: 1, path: 'Brandon Sanderson/Well', how: 'import' },
                import: { job_id: 4, state: 'done', code: '' },
              }),
            ],
            next: '',
          },
        },
      }),
    );
    renderApp('/server?topic=audible');
    const list = await screen.findByRole('list', { name: 'Audible titles' });
    const rows = within(list).getAllByRole('listitem');
    expect(within(rows[0]).queryByRole('link', { name: 'On this server' })).not.toBeInTheDocument();
    expect(within(rows[0]).getByText('Not here')).toBeInTheDocument();
    expect(
      within(rows[0]).getByRole('button', { name: 'Undo Not this book: The Final Empire' }),
    ).toBeInTheDocument();
    expect(within(rows[1]).getByRole('link', { name: 'On this server' })).toBeInTheDocument();
    expect(
      within(rows[1]).queryByRole('button', { name: /Not this book/ }),
    ).not.toBeInTheDocument();
  });

  it('says what was in the way of a backup that failed exists', () => {
    const strings = en as Record<string, string>;
    const failed = (conflict?: string) =>
      importStatus(title({ import: { job_id: 4, state: 'failed', code: 'exists', conflict } }))
        ?.code;
    expect(failed()).toBe('audible.code.exists');
    expect(failed('file')).toBe('audible.code.exists');
    expect(failed('book')).toBe('audible.code.exists.book');
    expect(failed('audio')).toBe('audible.code.exists.audio');
    expect(failed('something_new')).toBe('audible.code.exists');
    for (const key of ['audible.code.exists.book', 'audible.code.exists.audio'])
      expect(strings[key]).toBeTruthy();
  });

  it("saves an account's policy, template and sync", async () => {
    const calls = mockFetch(
      routes(true, {
        'PATCH /admin/audible/accounts/acct0001': (req) => ({
          body: { account: account(req.body as object) },
        }),
      }),
    );
    renderApp('/server?topic=bridge');
    const heading = await screen.findByRole('heading', { name: 'Audible accounts' });
    const card = heading.closest('section') as HTMLElement;
    expect(await within(card).findByText("Beside the series' other books")).toBeInTheDocument();
    const user = userEvent.setup();
    await user.click(within(card).getByRole('button', { name: 'Account settings...' }));
    const dialog = await screen.findByRole('dialog', { name: 'Settings for Chris (US)' });
    await user.selectOptions(
      within(dialog).getByRole('combobox', { name: 'Back up by itself' }),
      'series',
    );
    await user.type(
      within(dialog).getByRole('textbox', { name: 'Placement template' }),
      '{{Author}/{{Title}',
    );
    const sync = within(dialog).getByRole('switch', { name: 'Sync listening places with Audible' });
    expect(sync).toBeChecked();
    await user.click(sync);
    await user.click(within(dialog).getByRole('button', { name: 'Save' }));
    await waitFor(() => {
      const patch = calls.find((c) => c.method === 'PATCH');
      expect(patch?.body).toEqual({ policy: 'series', template: '{Author}/{Title}', sync: false });
    });
    expect(await screen.findByText('Settings for Chris (US) saved')).toBeInTheDocument();
  });
});

describe('needs a look', () => {
  const quiet = plan({ kind: 'standalone', siblings: 0 });
  const newAuthor = plan({ kind: 'standalone', siblings: 0, state: 'new' });

  it('keeps the titles not here whose planned backup warns', () => {
    expect(needsLook(title({ match: null, planned: newAuthor }), '')).toBe(true);
    expect(needsLook(title({ match: null, planned: quiet }), '')).toBe(false);
    // Here already, or nothing to back up: never listed.
    expect(needsLook(title({ planned: newAuthor }), '')).toBe(false);
    expect(needsLook(title({ match: null, state: 'gone', planned: newAuthor }), '')).toBe(false);
    expect(needsLook(title({ match: null, state: 'unavailable', planned: newAuthor }), '')).toBe(
      false,
    );
    // A destination of the admin's own isn't the plan's to warn about.
    // An own destination no longer valid needs a look whatever was planned.
    expect(
      needsLook(
        title({ match: null, planned: quiet, dest_path: '.Mine/Book', dest_invalid: true }),
        '',
      ),
    ).toBe(true);
    expect(needsLook(title({ match: null, planned: newAuthor, dest_path: 'Mine/Book' }), '')).toBe(
      false,
    );
  });

  it('lists preorders first, else in order', () => {
    const a = title({ asin: 'A', match: null, planned: newAuthor });
    const b = title({ asin: 'B', match: null, planned: quiet });
    const c = title({ asin: 'C', match: null, state: 'preorder', planned: newAuthor });
    const d = title({ asin: 'D', match: null, planned: plan({ kind: 'series_new', siblings: 0 }) });
    const got = lookList([a, b, c, d], new Map([['acct0001', '']]));
    expect(got.map((x) => x.asin)).toEqual(['C', 'A', 'D']);
  });
});

describe('unmatched titles, explained', () => {
  it('words a planned destination, its warnings and a stale template', () => {
    expect(plannedLine(title({ match: null }), '')).toBeNull();
    expect(plannedLine(title({ match: null, planned: plan() }), '')).toEqual({
      key: 'audible.planned.seriesExisting',
      values: { path: plan().path, count: 7 },
      warn: false,
      stale: false,
    });
    // A new series warns: the series may be here under another name (server#372).
    expect(
      plannedLine({ planned: plan({ kind: 'series_new', siblings: 0 }), dest_path: '' }, ''),
    ).toMatchObject({ key: 'audible.planned.seriesNew', warn: true });
    expect(
      plannedLine({ planned: plan({ kind: 'standalone', siblings: 0 }), dest_path: '' }, '')?.key,
    ).toBe('audible.planned.joinsAuthor');
    // With a template, joins only says the top folder already exists.
    expect(
      plannedLine(
        {
          planned: plan({ kind: 'standalone', siblings: 0, template: '{Author}/{Title}' }),
          dest_path: '',
        },
        '{Author}/{Title}',
      )?.key,
    ).toBe('audible.planned.joinsFolder');
    expect(
      plannedLine(
        {
          planned: plan({ kind: 'series_existing', siblings: 0, template: '{Narrator}/{Title}' }),
          dest_path: '',
        },
        '{Narrator}/{Title}',
      )?.key,
    ).toBe('audible.planned.joinsFolder');
    // Siblings still win over a template.
    expect(
      plannedLine({ planned: plan({ template: '{Author}/{Series}/{Title}' }), dest_path: '' }, '')
        ?.key,
    ).toBe('audible.planned.seriesExisting');
    // A new top folder warns: with the automatic placement it is a new author
    // folder, with a template just a new folder (server#372).
    expect(
      plannedLine(
        { planned: plan({ kind: 'standalone', siblings: 0, state: 'new' }), dest_path: '' },
        '',
      ),
    ).toMatchObject({ key: 'audible.planned.newAuthor', warn: true });
    expect(
      plannedLine(
        {
          planned: plan({ kind: 'series_new', siblings: 0, state: 'new', template: '{Title}' }),
          dest_path: '',
        },
        '{Title}',
      ),
    ).toMatchObject({ key: 'audible.planned.newFolder', warn: true });
    // A title joining an author folder, with no series of its own, is quiet.
    expect(
      plannedLine({ planned: plan({ kind: 'standalone', siblings: 0 }), dest_path: '' }, ''),
    ).toMatchObject({ key: 'audible.planned.joinsAuthor', warn: false });
    const exists = plannedLine(
      { planned: plan({ state: 'exists', conflict: 'book' }), dest_path: '' },
      '',
    );
    expect(exists).toMatchObject({ key: 'audible.planned.exists.book', warn: true });
    const failed = plannedLine(
      {
        planned: plan({ state: 'error', path: '', kind: '', conflict: 'unknown variable {Nope}' }),
        dest_path: '',
      },
      '',
    );
    expect(failed).toMatchObject({
      key: 'audible.planned.error',
      values: { message: 'unknown variable {Nope}' },
      warn: true,
    });
    // The account's template changed since the plan; unknown: no note.
    expect(plannedLine({ planned: plan(), dest_path: '' }, '{Author}/{Title}')?.stale).toBe(true);
    expect(plannedLine({ planned: plan(), dest_path: '' }, undefined)?.stale).toBe(false);
    // A destination of the admin's own replaces the plan.
    expect(plannedLine({ planned: plan(), dest_path: 'Mine/Book.m4b' }, '')).toBeNull();
    expect(plannedPlaceholder({ planned: plan() })).toBe(plan().path);
    expect(plannedPlaceholder({ planned: null })).toBe('Author/Series/Title.m4b');
    expect(reasonKey('contested')).toBeNull();
    expect(reasonKey('no_candidate')).toBe('audible.reason.other');
    expect(reasonKey('Not released yet (pre-order)')).toBeNull();
    expect(reasonKey('')).toBeNull();
  });

  it('shows where a backup would go, and warns when that place is taken', async () => {
    mockFetch(
      routes(true, {
        'GET /admin/audible/accounts': {
          body: { accounts: [account({ template: '{Author}/{Title}' })] },
        },
        'GET /admin/audible/titles': {
          body: {
            titles: [
              title({ asin: 'B000000001', title: 'Moon over Soho', match: null, planned: plan() }),
              title({
                asin: 'B000000002',
                title: 'Whispers Under Ground',
                match: null,
                planned: plan({
                  path: 'Ben Aaronovitch/Whispers Under Ground.m4b',
                  state: 'exists',
                  conflict: 'file',
                  template: '{Author}/{Title}',
                }),
              }),
              title({
                asin: 'B000000003',
                title: 'Broken Homes',
                match: null,
                dest_path: 'Mine/Broken Homes.m4b',
                planned: plan({ path: 'Ben Aaronovitch/Broken Homes.m4b' }),
              }),
              title({
                asin: 'B000000004',
                title: 'Foxglove Summer',
                state: 'unavailable',
                reason: 'Audible reports no download rights for this title',
                match: null,
                planned: plan({ path: 'Ben Aaronovitch/Foxglove Summer.m4b' }),
              }),
            ],
            next: '',
          },
        },
      }),
    );
    renderApp('/server?topic=audible');
    const list = await screen.findByRole('list', { name: 'Audible titles' });
    const rows = within(list).getAllByRole('listitem');
    expect(
      within(rows[0]).getByText(`Will go to ${plan().path}, beside 7 books of its series`, {
        exact: false,
      }),
    ).toBeInTheDocument();
    expect(within(rows[0]).getAllByText(/planned at the last check/).length).toBeGreaterThan(0);
    // The plan used the automatic placement; the account has a template now.
    expect(within(rows[0]).getByText(/changes at the next check/)).toBeInTheDocument();
    expect(
      within(rows[1]).getByText(/a file is already there, so the backup would stop/),
    ).toBeInTheDocument();
    expect(within(rows[1]).queryByText(/changes at the next check/)).not.toBeInTheDocument();
    // An override: checked when the backup starts, no plan shown.
    expect(within(rows[2]).getByText(/checked when the backup starts/)).toBeInTheDocument();
    expect(within(rows[2]).queryByText(/Will go to/)).not.toBeInTheDocument();
    // A title that can't be backed up shows no plan.
    expect(within(rows[3]).queryByText(/Will go to/)).not.toBeInTheDocument();
    // The destination dialog suggests the planned path, never fills it in.
    const user = userEvent.setup();
    await user.click(
      within(rows[0]).getByRole('button', { name: 'Destination for Moon over Soho' }),
    );
    const dialog = await screen.findByRole('dialog', { name: 'Where Moon over Soho goes' });
    const field = within(dialog).getByRole('textbox');
    expect(field).toHaveAttribute('placeholder', plan().path);
    expect(field).toHaveValue('');
  });

  it('explains a contested title, with its book and the other titles on it', async () => {
    const book = 'Ben Aaronovitch/PG02 - Moon over Soho';
    mockFetch(
      routes(true, {
        'GET /admin/audible/titles': {
          body: {
            titles: [
              title({
                asin: 'B000000001',
                title: 'Rivers of London',
                match: null,
                reason: 'contested',
                contested: {
                  library_id: 1,
                  path: book,
                  with: [
                    { asin: 'B000000002', title: 'Moon over Soho' },
                    { asin: 'B000000003', title: 'Whispers Under Ground' },
                  ],
                },
              }),
              title({
                asin: 'B000000004',
                title: 'Foxglove Summer',
                match: null,
                reason: 'contested',
              }),
              title({
                asin: 'B000000005',
                title: 'Lies Sleeping',
                match: null,
                reason: 'no_candidate',
              }),
            ],
            next: '',
          },
        },
      }),
    );
    renderApp('/server?topic=audible');
    const list = await screen.findByRole('list', { name: 'Audible titles' });
    const rows = within(list).getAllByRole('listitem');
    expect(
      within(rows[0]).getByText(
        /Matched the same book as Moon over Soho and Whispers Under Ground/,
      ),
    ).toBeInTheDocument();
    expect(within(rows[0]).getByRole('link', { name: book })).toHaveAttribute(
      'href',
      expect.stringContaining('/admin/library/book?library=1&path=Ben'),
    );
    expect(within(rows[0]).queryByText('contested')).not.toBeInTheDocument();
    expect(
      within(rows[1]).getByText(
        "Another Audible title matched the same book, so this one isn't shown as here.",
      ),
    ).toBeInTheDocument();
    expect(
      within(rows[2]).getByText("The Bridge couldn't match it to a book on this server."),
    ).toBeInTheDocument();
    expect(within(rows[2]).queryByText('no_candidate')).not.toBeInTheDocument();
  });
});

describe('audible sync', () => {
  it('words what a place sync did: only waited, found the sync off, or the places read', () => {
    const sync = (state: BridgeJob['state'], result: Record<string, unknown> | null) =>
      syncSummary({ kind: 'audible.sync', state, result });
    expect(sync('done', { titles: 4, waiting: true, wait_until: '2026-11-19T10:00:00Z' })).toEqual({
      kind: 'waiting',
      until: '2026-11-19T10:00:00Z',
    });
    expect(sync('done', { titles: 4, waiting: true })).toEqual({ kind: 'waiting' });
    expect(sync('done', { titles: 4, waiting: true, wait_until: 'soon' })).toEqual({
      kind: 'waiting',
    });
    expect(sync('done', { titles: 4, sync_off: true })).toEqual({ kind: 'off' });
    expect(sync('done', { titles: 4, heard: 3, applied: 1 })).toEqual({
      kind: 'counts',
      titles: 4,
      heard: 3,
      applied: 1,
      partial: false,
    });
    // A failed sync's counts are what it did before it stopped.
    expect(sync('failed', { code: 'audible', titles: 4, heard: 2, applied: 2 })).toMatchObject({
      kind: 'counts',
      partial: true,
    });
    expect(sync('failed', { code: 'audible', message: 'no answer' })).toBeUndefined();
    expect(sync('running', { titles: 4, heard: 1 })).toBeUndefined();
    expect(
      syncSummary({ kind: 'audible.poll', state: 'done', result: { titles: 4, heard: 1 } }),
    ).toBeUndefined();
  });

  it("says in the Bridge's jobs that a done sync only waited on Audible", async () => {
    const ended = (id: number, state: BridgeJob['state'], result: Record<string, unknown>) =>
      ({
        id,
        kind: 'audible.sync',
        library_id: null,
        path: null,
        params: { account: 'acct0001' },
        state,
        progress: {},
        result,
        error: state === 'failed' ? 'Audible stopped answering' : '',
        requested_by: null,
        priority: -10,
        attempts: 1,
        lease_until: null,
        cancel_requested: false,
        created_at: '2026-11-19T09:00:00Z',
        updated_at: '2026-11-19T09:01:00Z',
        claimed_at: '2026-11-19T09:00:05Z',
        finished_at: '2026-11-19T09:01:00Z',
      }) as BridgeJob;
    mockFetch(
      routes(true, {
        'GET /admin/bridge/jobs': {
          body: {
            jobs: [
              ended(1, 'done', { titles: 4, waiting: true }),
              ended(2, 'done', { titles: 4, heard: 3, applied: 1 }),
              ended(3, 'failed', { code: 'audible', titles: 4, heard: 2, applied: 2 }),
            ],
            next_before: 0,
          },
        },
      }),
    );
    renderApp('/server?topic=bridge');
    const jobs = await screen.findByRole('list', { name: 'Jobs' });
    expect(
      within(jobs).getByText('Audible asked the Bridge to wait, so nothing was synced.'),
    ).toBeInTheDocument();
    expect(
      within(jobs).getByText('4 titles checked · 3 with a place on Audible · 1 moved here.'),
    ).toBeInTheDocument();
    expect(
      within(jobs).getByText(
        'Before it stopped: 4 titles checked · 2 with a place on Audible · 2 moved here.',
      ),
    ).toBeInTheDocument();
  });

  it('lets a title sync only while it is on this server and not left out', () => {
    expect(canSync(title())).toBe(true);
    expect(canSync(title({ match: null }))).toBe(false);
    expect(canSync(title({ rejected: true }))).toBe(false);
    expect(canSync(title({ state: 'gone' }))).toBe(false);
    expect(canSync(title({ state: 'preorder' }))).toBe(false);
    expect(canSync(title({ state: 'unavailable' }))).toBe(true);
    expect(canSync(title({ ignored: true }))).toBe(false);
    expect(canSync(title({ no_sync: true }))).toBe(false);
  });

  it("says whether each account's places sync, paused or refused", async () => {
    mockFetch(
      routes(true, {
        'GET /admin/audible/accounts': {
          body: {
            accounts: [
              account({ id: 'acct0001', label: 'On', last_sync_at: new Date().toISOString() }),
              account({ id: 'acct0002', label: 'Second', sync: false }),
              account({ id: 'acct0003', label: 'Refused', refused_at: new Date().toISOString() }),
              account({ id: 'acct0004', label: 'New' }),
              account({
                id: 'acct0005',
                label: 'Paused',
                paused: true,
                library_id: null,
                library: null,
              }),
            ],
          },
        },
      }),
    );
    renderApp('/server?topic=bridge');
    const heading = await screen.findByRole('heading', { name: 'Audible accounts' });
    const card = heading.closest('section') as HTMLElement;
    expect(await within(card).findByText(/^On, synced /)).toBeInTheDocument();
    expect(within(card).getByText('Off')).toBeInTheDocument();
    expect(
      within(card).getByText('Stopped: Audible refused the account. Sign in again on the Bridge.'),
    ).toBeInTheDocument();
    expect(within(card).getByText('On, not synced yet')).toBeInTheDocument();
    expect(within(card).getByText('On, paused with the account')).toBeInTheDocument();
  });

  it('warns on a refused account instead of showing it checked (bridge#40)', async () => {
    mockFetch(
      routes(true, {
        'GET /admin/audible/accounts': {
          body: {
            accounts: [
              account({ id: 'acct0001', label: 'Fine' }),
              account({
                id: 'acct0002',
                label: 'Refused',
                sync: false,
                refused_at: new Date().toISOString(),
              }),
            ],
          },
        },
      }),
    );
    renderApp('/server?topic=bridge');
    const heading = await screen.findByRole('heading', { name: 'Audible accounts' });
    const card = heading.closest('section') as HTMLElement;
    const fine = (await within(card).findByText('Fine')).closest('li') as HTMLElement;
    const refused = within(card).getByText('Refused').closest('li') as HTMLElement;
    expect(within(fine).getByText(/^Checked /)).toBeInTheDocument();
    expect(within(refused).getByText('Refused: sign in again on the Bridge')).toBeInTheDocument();
    expect(within(refused).queryByText(/^Checked /)).toBeNull();
  });

  it("marks a title Don't sync, shows it, and undoes it", async () => {
    const calls = mockFetch(
      routes(true, {
        'GET /admin/audible/titles': {
          body: {
            titles: [
              title(),
              title({ asin: 'B000000002', title: 'Left Out', no_sync: true }),
              title({ asin: 'B000000003', title: 'Not Here', match: null }),
            ],
            next: '',
          },
        },
        'PATCH /admin/audible/titles/acct0001/B000000001': { body: title({ no_sync: true }) },
        'PATCH /admin/audible/titles/acct0001/B000000002': {
          body: title({ asin: 'B000000002', title: 'Left Out', no_sync: false }),
        },
      }),
    );
    renderApp('/server?topic=audible');
    const user = userEvent.setup();
    await user.click(
      await screen.findByRole('button', { name: "Don't sync this book: The Final Empire" }),
    );
    await waitFor(() =>
      expect(
        calls.some(
          (c) =>
            c.method === 'PATCH' &&
            c.path.endsWith('/B000000001') &&
            (c.body as { no_sync?: boolean }).no_sync === true,
        ),
      ).toBe(true),
    );
    // The row takes the server's answer: both rows are left out now.
    expect(screen.getAllByText('Not synced')).toHaveLength(2);
    await user.click(screen.getByRole('button', { name: 'Sync this book again: Left Out' }));
    await waitFor(() =>
      expect(
        calls.some(
          (c) =>
            c.method === 'PATCH' &&
            c.path.endsWith('/B000000002') &&
            (c.body as { no_sync?: boolean }).no_sync === false,
        ),
      ).toBe(true),
    );
    expect(
      screen.queryByRole('button', { name: "Don't sync this book: Not Here" }),
    ).not.toBeInTheDocument();
  });
});

describe('storeTitle', () => {
  it('keeps the answered row over a list read already under way', async () => {
    const qc = new QueryClient();
    const key = [...keys.audibleTitleLists, 'acct'];
    const before = title({ can_backup: true });
    const after = title({ can_backup: false, import: { job_id: 7, state: 'queued', code: '' } });
    const other = title({ asin: 'B000000009', title: 'Other' });
    const page = (x: AudibleTitle): AudibleTitlePage => ({ titles: [x], next: '' });
    qc.setQueryData<InfiniteData<AudibleTitlePage>>(key, {
      pages: [page(before)],
      pageParams: [''],
    });
    const otherKey = [...keys.audibleTitleLists, 'other'];
    const otherData = { pages: [page(other)], pageParams: [''] };
    qc.setQueryData<InfiniteData<AudibleTitlePage>>(otherKey, otherData);
    // The poll's read started before the answer and lands after it, with the
    // row as it was.
    let land!: (p: AudibleTitlePage) => void;
    const read = qc
      .fetchInfiniteQuery({
        queryKey: key,
        queryFn: () => new Promise<AudibleTitlePage>((resolve) => (land = resolve)),
        initialPageParam: '',
      })
      .catch(() => undefined);
    await storeTitle(qc, after);
    land(page(before));
    await read;
    const got = qc.getQueryData<InfiniteData<AudibleTitlePage>>(key);
    expect(got?.pages[0]?.titles[0]).toEqual(after);
    // A list without the title is left as it was.
    expect(qc.getQueryData(otherKey)).toBe(otherData);
  });

  it("starts a shown list's stopped read again, past the answer", async () => {
    const qc = new QueryClient();
    const key = [...keys.audibleTitleLists, 'acct'];
    const before = title({ no_sync: false });
    const after = title({ no_sync: true });
    const bought = title({ asin: 'B000000009', title: 'Bought since' });
    qc.setQueryData<InfiniteData<AudibleTitlePage>>(key, {
      pages: [{ titles: [before], next: '' }],
      pageParams: [''],
    });
    // The list is on screen, and a check that landed has it reading again:
    // the first read never answers, the one started after the answer does.
    let reads = 0;
    const shown = new InfiniteQueryObserver<AudibleTitlePage>(qc, {
      queryKey: key,
      queryFn: () =>
        ++reads === 1
          ? new Promise<AudibleTitlePage>(() => undefined)
          : Promise.resolve({ titles: [after, bought], next: '' }),
      initialPageParam: '',
      getNextPageParam: () => undefined,
    });
    const unsubscribe = shown.subscribe(() => undefined);
    await waitFor(() => expect(reads).toBe(1));
    await storeTitle(qc, after);
    await waitFor(() =>
      expect(qc.getQueryData<InfiniteData<AudibleTitlePage>>(key)?.pages[0]?.titles).toEqual([
        after,
        bought,
      ]),
    );
    expect(reads).toBe(2);
    unsubscribe();
  });
});
