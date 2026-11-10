import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { keys } from '@/api/hooks';
import { setToken } from '@/api/token';
import type {
  AudibleAccount,
  AudiblePlan,
  AudibleTitle,
  BridgeJob,
  BridgeStatus,
} from '@/api/types';
import { mockFetch, type MockReply, type MockRoute } from '@/test/fetch-mock';
import { settingsWith } from '@/test/fixtures';
import { renderApp } from '@/test/render-app';
import { signedInRoutes } from '@/test/routes';
import en from '@/i18n/locales/en.json';
import {
  accountName,
  audibleJobReason,
  backupRunning,
  canBackUp,
  canSync,
  chosenAccount,
  cleanDest,
  importStatus,
  matchedParam,
  plannedLine,
  plannedPlaceholder,
  pollTarget,
  reasonKey,
  titleByline,
} from './audible-model';

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
        'GET /admin/audible/titles': { body: { titles: [title({ match: null })], next: '' } },
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
          'GET /admin/audible/titles': { body: { titles: [title({ match: null })], next: '' } },
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
  it('offers Back up only for a title it can download and nobody is backing up', () => {
    const missing = title({ match: null });
    expect(canBackUp(missing)).toBe(true);
    expect(canBackUp(title())).toBe(false);
    expect(canBackUp(title({ rejected: true }))).toBe(true);
    expect(canBackUp(title({ match: null, state: 'preorder' }))).toBe(true);
    expect(canBackUp(title({ match: null, state: 'unavailable' }))).toBe(false);
    expect(canBackUp(title({ match: null, state: 'gone' }))).toBe(false);
    for (const state of ['pending', 'queued', 'claimed', 'running', 'needs_attention'] as const) {
      expect(canBackUp(title({ match: null, import: { job_id: 4, state, code: '' } }))).toBe(false);
    }
    expect(
      canBackUp(title({ match: null, import: { job_id: 4, state: 'failed', code: 'exists' } })),
    ).toBe(true);
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
  });

  it('backs a title up, and says a preorder waits for its release', async () => {
    let pending = false;
    const calls = mockFetch(
      routes(true, {
        'GET /admin/audible/titles': {
          body: {
            titles: [
              title({ asin: 'B000000002', title: 'The Well of Ascension', match: null }),
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
        'GET /admin/audible/titles': { body: { titles: [title({ match: null })], next: '' } },
      }),
    );
    renderApp('/server?topic=audible');
    const list = await screen.findByRole('list', { name: 'Audible titles' });
    expect(within(list).getByRole('button', { name: 'Back up' })).toBeDisabled();
  });

  it('marks a title Not this book and sets where it goes', async () => {
    const calls = mockFetch(
      routes(true, {
        'GET /admin/audible/titles': { body: { titles: [title({ match: null })], next: '' } },
        'PATCH /admin/audible/titles/acct0001/B000000001': (req) => ({
          body: { title: title({ match: null, ...(req.body as object) }) },
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

describe('unmatched titles, explained', () => {
  it('words a planned destination, its warnings and a stale template', () => {
    expect(plannedLine(title({ match: null }), '')).toBeNull();
    expect(plannedLine(title({ match: null, planned: plan() }), '')).toEqual({
      key: 'audible.planned.seriesExisting',
      values: { path: plan().path, count: 7 },
      warn: false,
      stale: false,
    });
    expect(
      plannedLine({ planned: plan({ kind: 'series_new', siblings: 0 }), dest_path: '' }, '')?.key,
    ).toBe('audible.planned.seriesNew');
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
    expect(
      plannedLine(
        { planned: plan({ kind: 'standalone', siblings: 0, state: 'new' }), dest_path: '' },
        '',
      )?.key,
    ).toBe('audible.planned.standalone');
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
        'PATCH /admin/audible/titles/acct0001/B000000002': { body: title({ no_sync: false }) },
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
    expect(screen.getByText('Not synced')).toBeInTheDocument();
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
