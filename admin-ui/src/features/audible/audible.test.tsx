import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { setToken } from '@/api/token';
import type { AudibleAccount, AudibleTitle, BridgeJob, BridgeStatus } from '@/api/types';
import { mockFetch, type MockRoute } from '@/test/fetch-mock';
import { settingsWith } from '@/test/fixtures';
import { renderApp } from '@/test/render-app';
import { signedInRoutes } from '@/test/routes';
import { accountName, matchedParam, pollTarget, titleByline } from './audible-model';

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
    dest_path: '',
    first_seen: '2026-11-01T09:00:00Z',
    updated_at: '2026-11-01T09:00:00Z',
    import_job_id: null,
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
  kinds: ['bridge.verify', 'audible.poll'],
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

describe('audible model', () => {
  it('words the filters and names', () => {
    expect(matchedParam('matched')).toBe(1);
    expect(matchedParam('missing')).toBe(0);
    expect(matchedParam('')).toBeUndefined();
    const a = account();
    const b = account({ id: 'acct0002' });
    expect(pollTarget([a], '')).toBe(a);
    expect(pollTarget([a, b], '')).toBeUndefined();
    expect(pollTarget([a, b], 'acct0002')).toBe(b);
    expect(titleByline(title())).toBe('Brandon Sanderson · Mistborn 1');
    expect(titleByline(title({ authors: [], series: [] }))).toBe('');
    const t = ((k: string, o?: Record<string, string>) => `${k}:${o?.name}:${o?.store}`) as never;
    expect(accountName(t, account({ label: '' }))).toBe('audible.account.unnamed:Chris:US');
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
                library_id: null,
                library: null,
                paused: true,
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
    expect(within(rows[1]).getByText('Paused')).toBeInTheDocument();
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
    renderApp('/server?topic=audible');
    const list = await screen.findByRole('list', { name: 'Audible titles' });
    const rows = within(list).getAllByRole('listitem');
    expect(rows).toHaveLength(2);
    expect(within(rows[0]).getByRole('link', { name: 'On this server' })).toHaveAttribute(
      'href',
      expect.stringContaining('/admin/library/book?library=1&path=Brandon'),
    );
    expect(within(rows[1]).getByText('Preorder')).toBeInTheDocument();
    expect(within(rows[1]).getByText('Not here')).toBeInTheDocument();
    expect(within(rows[1]).getByText(/Releases/)).toBeInTheDocument();
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
    // One account: Check now polls it, no account picker.
    expect(screen.queryByRole('combobox', { name: 'Account' })).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Check now' })).toBeEnabled();
  });

  it('says so when a filter matches nothing', async () => {
    mockFetch(routes(true, { 'GET /admin/audible/titles': { body: { titles: [], next: '' } } }));
    renderApp('/server?topic=audible');
    expect(await screen.findByText('No titles yet')).toBeInTheDocument();
  });

  it('opens General while the module is off', async () => {
    mockFetch(routes(false));
    renderApp('/server?topic=audible');
    expect(await screen.findByRole('textbox', { name: 'Server name' })).toBeInTheDocument();
  });
});
