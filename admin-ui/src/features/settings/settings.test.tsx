import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { setToken } from '@/api/token';
import type { AdminSettings } from '@/api/types';
import { mockFetch, type MockRoute } from '@/test/fetch-mock';
import { mirrorStatus, mirrorSystem, settingsWith, systemStatus } from '@/test/fixtures';
import { renderApp } from '@/test/render-app';
import { signedInRoutes } from '@/test/routes';

// Server > Settings: topics, forms that send only what changed, refusals on
// their field, locked and restart settings, and the switches that save at once.

function routes(over: Record<string, MockRoute> = {}) {
  return signedInRoutes({
    'GET /admin/system': { body: systemStatus() },
    ...over,
  });
}

/** A PATCH that answers with the settings merged with what it sent. */
function patchEcho(base: AdminSettings, extra: Partial<AdminSettings> = {}): MockRoute {
  return (req) => {
    const body = req.body as Record<string, Record<string, unknown>>;
    const next = { ...base, ...extra } as unknown as Record<string, unknown>;
    for (const [section, fields] of Object.entries(body)) {
      next[section] = { ...(next[section] as object), ...fields };
    }
    return { body: next };
  };
}

beforeEach(() => setToken('stored'));

describe('settings', () => {
  it('opens on General and moves between topics', async () => {
    mockFetch(routes());
    const { router } = renderApp('/server');
    expect(await screen.findByRole('textbox', { name: 'Server name' })).toBeInTheDocument();
    const nav = screen.getByRole('navigation', { name: 'Settings topics' });
    expect(within(nav).getByRole('link', { name: 'General' })).toHaveAttribute(
      'aria-current',
      'page',
    );
    await userEvent.setup().click(within(nav).getByRole('link', { name: 'Demo mode' }));
    await waitFor(() => expect(router.state.location.search).toEqual({ topic: 'demo' }));
    expect(await screen.findByRole('switch', { name: 'Enable demo mode' })).toBeInTheDocument();
    // Only the open topic reads as current (General's empty search matches every topic).
    expect(within(nav).getByRole('link', { name: 'Demo mode' })).toHaveAttribute(
      'aria-current',
      'page',
    );
    expect(within(nav).getByRole('link', { name: 'General' })).not.toHaveAttribute('aria-current');
  });

  it('saves only the changed fields and says they apply now', async () => {
    const base = settingsWith();
    const calls = mockFetch(routes({ 'PATCH /admin/settings': patchEcho(base) }));
    renderApp('/server');
    const user = userEvent.setup();
    const name = await screen.findByRole('textbox', { name: 'Server name' });
    const save = within(name.closest('section') as HTMLElement).getByRole('button', {
      name: 'Save changes',
    });
    expect(save).toBeDisabled();
    await user.type(name, 'Hearthside');
    await user.click(save);
    expect(await screen.findByText('This server saved')).toBeInTheDocument();
    expect(screen.getByText('Applied now. No restart needed.')).toBeInTheDocument();
    expect(calls.find((c) => c.method === 'PATCH')?.body).toEqual({
      general: { name: 'Hearthside' },
    });
  });

  it('keeps listening sessions for the days set', async () => {
    const base = settingsWith();
    const calls = mockFetch(routes({ 'PATCH /admin/settings': patchEcho(base) }));
    renderApp('/server');
    const user = userEvent.setup();
    const days = await screen.findByRole('textbox', { name: 'Days to keep sessions' });
    expect(days).toHaveValue('400');
    await user.clear(days);
    await user.type(days, '90');
    await user.click(
      within(days.closest('section') as HTMLElement).getByRole('button', { name: 'Save changes' }),
    );
    expect(await screen.findByText('Listening history saved')).toBeInTheDocument();
    expect(calls.find((c) => c.method === 'PATCH')?.body).toEqual({
      general: { session_days: 90 },
    });
  });

  it('saves the home address under the public one, and locks it to its variable', async () => {
    const base = settingsWith();
    const calls = mockFetch(routes({ 'PATCH /admin/settings': patchEcho(base) }));
    renderApp('/server');
    const user = userEvent.setup();
    const home = await screen.findByRole('textbox', { name: 'Home address' });
    expect(home).toHaveAttribute('placeholder', 'http://192.168.1.20:8080');
    expect(home.closest('section')).toBe(
      screen.getByRole('textbox', { name: 'Public address' }).closest('section'),
    );
    await user.type(home, 'http://192.168.1.20:8080');
    await user.click(
      within(home.closest('section') as HTMLElement).getByRole('button', { name: 'Save changes' }),
    );
    expect(await screen.findByText('This server saved')).toBeInTheDocument();
    expect(calls.find((c) => c.method === 'PATCH')?.body).toEqual({
      general: { lan_url: 'http://192.168.1.20:8080' },
    });
  });

  it('locks the home address the environment sets', async () => {
    mockFetch(
      routes({
        'GET /admin/settings': {
          body: settingsWith({
            general: {
              name: '',
              public_url: '',
              lan_url: 'http://nas.local:8080',
              update_check: true,
              session_days: 400,
            },
            locked: { 'general.lan_url': 'AUDIOSILO_LAN_URL' },
          }),
        },
      }),
    );
    renderApp('/server');
    expect(await screen.findByRole('textbox', { name: 'Home address' })).toBeDisabled();
    expect(screen.getByText('Set by AUDIOSILO_LAN_URL')).toBeInTheDocument();
  });

  it('shows a refusal on the field it names and keeps the edit', async () => {
    mockFetch(
      routes({
        'PATCH /admin/settings': {
          status: 400,
          body: {
            error: 'general.public_url: must be an address starting with https:// or http://',
            code: 'invalid_setting',
            field: 'general.public_url',
          },
        },
      }),
    );
    renderApp('/server');
    const user = userEvent.setup();
    const url = await screen.findByRole('textbox', { name: 'Public address' });
    await user.type(url, 'books.example.com');
    await user.click(
      within(url.closest('section') as HTMLElement).getByRole('button', { name: 'Save changes' }),
    );
    expect(await screen.findByRole('alert')).toHaveTextContent('must be an address starting with');
    expect(url).toHaveAttribute('aria-invalid', 'true');
    expect(url).toHaveValue('books.example.com');
  });

  it('locks a setting the environment sets', async () => {
    mockFetch(
      routes({
        'GET /admin/settings': {
          body: settingsWith({
            general: {
              name: '',
              public_url: 'https://env.example.com',
              lan_url: '',
              update_check: true,
              session_days: 400,
            },
            locked: { 'general.public_url': 'AUDIOSILO_PUBLIC_URL' },
          }),
        },
      }),
    );
    renderApp('/server');
    expect(await screen.findByRole('textbox', { name: 'Public address' })).toBeDisabled();
    expect(screen.getByText('Set by AUDIOSILO_PUBLIC_URL')).toBeInTheDocument();
  });

  it('offers no save on a card the environment sets entirely', async () => {
    mockFetch(
      routes({
        'GET /admin/settings': {
          body: settingsWith({
            network: { ...settingsWith().network, tls_mode: 'off' },
            locked: { 'network.tls_mode': 'AUDIOSILO_TLS_MODE' },
          }),
        },
      }),
    );
    renderApp('/server?topic=network');
    expect(await screen.findByText('Set by AUDIOSILO_TLS_MODE')).toBeInTheDocument();
    // Only the Network card (listen address, proxies, origins) can be saved.
    expect(screen.getAllByRole('button', { name: 'Save changes' })).toHaveLength(1);
  });

  it('asks before saving a restart setting, then lists it as waiting', async () => {
    const base = settingsWith();
    const calls = mockFetch(
      routes({
        'PATCH /admin/settings': patchEcho(base, { restart_pending: ['network.bind'] }),
      }),
    );
    renderApp('/server?topic=network');
    const user = userEvent.setup();
    const bind = await screen.findByRole('textbox', { name: 'Listen address' });
    await user.clear(bind);
    await user.type(bind, '0.0.0.0:9443');
    const card = bind.closest('section') as HTMLElement;
    await user.click(within(card).getByRole('button', { name: 'Save changes' }));
    const dialog = await screen.findByRole('dialog', {
      name: 'Save and apply at the next restart?',
    });
    await user.click(within(dialog).getByRole('button', { name: 'Save changes' }));
    expect(
      await screen.findByText('Restart AudioSilo to finish applying your changes'),
    ).toBeInTheDocument();
    expect(screen.getByText(/Listen address\. Until then/)).toBeInTheDocument();
    expect(calls.find((c) => c.method === 'PATCH')?.body).toEqual({
      network: { bind: '0.0.0.0:9443' },
    });
  });

  it("asks for certificate names only with Let's Encrypt, one per line", async () => {
    const base = settingsWith();
    const calls = mockFetch(routes({ 'PATCH /admin/settings': patchEcho(base) }));
    renderApp('/server?topic=network');
    const user = userEvent.setup();
    expect(await screen.findByText(/^Valid · [\d,]+ days left$/)).toBeInTheDocument();
    expect(screen.queryByRole('textbox', { name: 'Certificate names' })).not.toBeInTheDocument();
    await user.click(screen.getByRole('radio', { name: /Let's Encrypt/ }));
    const hosts = screen.getByRole('textbox', { name: 'Certificate names' });
    await user.type(hosts, 'books.example.com{Enter}audio.example.com');
    const card = hosts.closest('section') as HTMLElement;
    await user.click(within(card).getByRole('button', { name: 'Save changes' }));
    await user.click(
      within(await screen.findByRole('dialog')).getByRole('button', { name: 'Save changes' }),
    );
    await waitFor(() =>
      expect(calls.find((c) => c.method === 'PATCH')?.body).toEqual({
        network: { tls_mode: 'autocert', tls_hosts: ['books.example.com', 'audio.example.com'] },
      }),
    );
  });

  it('turns the update check off at once', async () => {
    const base = settingsWith();
    const calls = mockFetch(routes({ 'PATCH /admin/settings': patchEcho(base) }));
    renderApp('/server');
    const toggle = await screen.findByRole('switch', { name: 'Check for new versions' });
    expect(toggle).toBeChecked();
    await userEvent.setup().click(toggle);
    expect(await screen.findByText('Update check is off')).toBeInTheDocument();
    expect(calls.find((c) => c.method === 'PATCH')?.body).toEqual({
      general: { update_check: false },
    });
  });

  it('turns community metadata off', async () => {
    const base = settingsWith();
    const calls = mockFetch(routes({ 'PATCH /admin/settings': patchEcho(base) }));
    renderApp('/server?topic=metadata');
    const toggle = await screen.findByRole('switch', { name: 'Look up community metadata' });
    expect(toggle).toBeChecked();
    expect(await screen.findByText('Responding · 84 ms')).toBeInTheDocument();
    await userEvent.setup().click(toggle);
    expect(await screen.findByText('Community metadata is off')).toBeInTheDocument();
    expect(calls.find((c) => c.method === 'PATCH')?.body).toEqual({ metadata: { enabled: false } });
    expect(toggle).not.toBeChecked();
  });

  it('sets the preferred Audible marketplace', async () => {
    const base = settingsWith();
    const calls = mockFetch(routes({ 'PATCH /admin/settings': patchEcho(base) }));
    const user = userEvent.setup();
    renderApp('/server?topic=metadata');
    const select = await screen.findByRole('combobox', { name: /Audible marketplace/ });
    expect(select).toHaveValue('');
    await user.selectOptions(select, 'United Kingdom (audible.co.uk)');
    await user.click(
      within(select.closest('section') as HTMLElement).getByRole('button', {
        name: 'Save changes',
      }),
    );
    await waitFor(() =>
      expect(calls.find((c) => c.method === 'PATCH')?.body).toEqual({ metadata: { region: 'uk' } }),
    );
  });

  it('switches to a local copy, applied at the next restart', async () => {
    const base = settingsWith();
    const calls = mockFetch(
      routes({
        'PATCH /admin/settings': patchEcho(base, { restart_pending: ['metadata.mode'] }),
      }),
    );
    renderApp('/server?topic=metadata');
    const user = userEvent.setup();
    const remote = await screen.findByRole('radio', {
      name: /Ask the metadata service for each book/,
    });
    expect(remote).toBeChecked();
    const mirror = screen.getByRole('radio', { name: /Keep a local copy/ });
    // The cost and the privacy gain are on the choice itself.
    expect(mirror).toHaveAccessibleName(/About 1\.8 GB on disk/);
    expect(mirror).toHaveAccessibleName(/No book is looked up over the internet/);
    await user.click(mirror);
    const card = mirror.closest('section') as HTMLElement;
    expect(within(card).getByText('Restart to apply')).toBeInTheDocument();
    await user.click(within(card).getByRole('button', { name: 'Save changes' }));
    expect(
      await screen.findByText('Restart AudioSilo to finish applying your changes'),
    ).toBeInTheDocument();
    expect(screen.getByText(/next start: Metadata source\. Until then/)).toBeInTheDocument();
    expect(within(card).getByText('Waiting for a restart')).toBeInTheDocument();
    expect(calls.find((c) => c.method === 'PATCH')?.body).toEqual({
      metadata: { mode: 'mirror' },
    });
  });

  it("shows the local copy's state as the status in mirror mode", async () => {
    mockFetch(
      routes({
        'GET /admin/settings': {
          body: settingsWith({ metadata: { ...settingsWith().metadata, mode: 'mirror' } }),
        },
        'GET /admin/system': {
          body: mirrorSystem(mirrorStatus({ state: 'downloading', fallback: true })),
        },
        'GET /admin/meta/mirror': {
          body: mirrorStatus({ state: 'downloading', fallback: true }),
        },
      }),
    );
    renderApp('/server?topic=metadata');
    expect(await screen.findByText('Local copy downloading')).toBeInTheDocument();
    expect(screen.queryByText(/Responding/)).not.toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Details in Health > System' })).toHaveAttribute(
      'href',
      '/admin/health/system',
    );
  });

  it('follows the local copy while it opens, as Health > System does', async () => {
    let systemGets = 0;
    const calls = mockFetch(
      routes({
        'GET /admin/settings': {
          body: settingsWith({ metadata: { ...settingsWith().metadata, mode: 'mirror' } }),
        },
        'GET /admin/system': () => ({
          body: mirrorSystem(
            ++systemGets === 1
              ? mirrorStatus({ state: 'opening', fallback: true, tag: undefined })
              : mirrorStatus(),
          ),
        }),
        'GET /admin/meta/mirror': { body: mirrorStatus() },
      }),
    );
    renderApp('/server?topic=metadata');
    expect(await screen.findByText('Opening the local copy')).toBeInTheDocument();
    expect(await screen.findByText('Local copy ready', {}, { timeout: 3000 })).toBeInTheDocument();
    expect(calls.some((c) => c.path === '/admin/meta/mirror')).toBe(true);
  });

  it("can't turn metadata on without a configured service, and says how to fix it", async () => {
    mockFetch(
      routes({
        'GET /admin/settings': {
          body: settingsWith({
            metadata: {
              enabled: false,
              base_url: '',
              region: '',
              mode: 'remote',
              available: false,
            },
          }),
        },
      }),
    );
    renderApp('/server?topic=metadata');
    const toggle = await screen.findByRole('switch', { name: 'Look up community metadata' });
    expect(toggle).toHaveAttribute('aria-disabled', 'true');
    expect(screen.getByText('No metadata service is configured')).toBeInTheDocument();
  });

  it('clears the community matches of one library once the word is typed', async () => {
    const calls = mockFetch(
      routes({
        'DELETE /admin/community-matches': { body: { books: 12, covers: 3, runs: 1 } },
      }),
    );
    const user = userEvent.setup();
    renderApp('/server?topic=metadata');
    const zone = await screen.findByRole('region', { name: 'Danger zone' });
    await user.selectOptions(
      await within(zone).findByRole('combobox', { name: 'Library to clear' }),
      'Kids',
    );
    await user.click(within(zone).getByRole('button', { name: 'Clear matches' }));
    const dialog = await screen.findByRole('dialog', {
      name: 'Clear the community matches in Kids?',
    });
    const confirm = within(dialog).getByRole('button', { name: 'Clear matches' });
    expect(confirm).toBeDisabled();
    await user.type(within(dialog).getByRole('textbox'), 'clear');
    await user.click(confirm);
    expect(await screen.findByText('12 books are back to their own metadata.')).toBeInTheDocument();
    const sent = calls.find((c) => c.method === 'DELETE');
    expect(sent?.query.get('library_id')).toBe('2');
  });

  it('says to wait while a match run is working', async () => {
    mockFetch(
      routes({
        'DELETE /admin/community-matches': {
          status: 409,
          body: { error: 'a match run is already working', code: 'match_run_busy' },
        },
      }),
    );
    const user = userEvent.setup();
    renderApp('/server?topic=metadata');
    const zone = await screen.findByRole('region', { name: 'Danger zone' });
    await user.click(within(zone).getByRole('button', { name: 'Clear matches' }));
    const dialog = await screen.findByRole('dialog', {
      name: 'Clear the community matches in every library?',
    });
    await user.type(within(dialog).getByRole('textbox'), 'clear');
    await user.click(within(dialog).getByRole('button', { name: 'Clear matches' }));
    expect(
      await within(dialog).findByText(
        'A match run is working. Wait for it to finish, or stop it, then try again.',
      ),
    ).toBeInTheDocument();
  });

  it('restores the switch when the change fails', async () => {
    mockFetch(
      routes({
        'PATCH /admin/settings': { status: 500, body: { error: 'could not save settings' } },
      }),
    );
    renderApp('/server?topic=metadata');
    const toggle = await screen.findByRole('switch', { name: 'Look up community metadata' });
    await userEvent.setup().click(toggle);
    expect(await screen.findByText('could not save settings')).toBeInTheDocument();
    expect(toggle).toBeChecked();
  });

  it('shows the tools the server found for transcoding', async () => {
    mockFetch(
      routes({
        'GET /admin/system': {
          body: systemStatus({
            tools: [
              { name: 'ffmpeg', path: '', version: '', source: '' },
              {
                name: 'ffprobe',
                path: '/data/tools/ffprobe',
                version: '7.0',
                source: 'downloaded',
              },
            ],
          }),
        },
      }),
    );
    renderApp('/server?topic=transcoding');
    expect(await screen.findByText('7.0 · downloaded')).toBeInTheDocument();
    expect(screen.getByText(/Not found, so books in formats/)).toBeInTheDocument();
  });

  it('sends a number field that is not a number as typed, for the server to refuse', async () => {
    const calls = mockFetch(
      routes({
        'PATCH /admin/settings': {
          status: 400,
          body: { error: 'wrong type of value', code: 'invalid_setting', field: 'demo.max_users' },
        },
      }),
    );
    renderApp('/server?topic=demo');
    const user = userEvent.setup();
    await user.type(
      await screen.findByRole('textbox', { name: 'Most guests at once' }),
      '50 users',
    );
    await user.click(screen.getByRole('button', { name: 'Save changes' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('wrong type of value');
    expect(calls.find((c) => c.method === 'PATCH')?.body).toEqual({
      demo: { max_users: '50 users' },
    });
  });

  it('never saves an edit to a field hidden since', async () => {
    const base = settingsWith();
    const calls = mockFetch(routes({ 'PATCH /admin/settings': patchEcho(base) }));
    renderApp('/server?topic=network');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('radio', { name: /Let's Encrypt/ }));
    await user.type(
      screen.getByRole('textbox', { name: 'Certificate names' }),
      'books.example.com',
    );
    await user.click(screen.getByRole('radio', { name: /^Off/ }));
    const card = screen.getByRole('radio', { name: /^Off/ }).closest('section') as HTMLElement;
    await user.click(within(card).getByRole('button', { name: 'Save changes' }));
    await user.click(
      within(await screen.findByRole('dialog')).getByRole('button', { name: 'Save changes' }),
    );
    await waitFor(() =>
      expect(calls.find((c) => c.method === 'PATCH')?.body).toEqual({
        network: { tls_mode: 'off' },
      }),
    );
  });

  it('offers the libraries for the demo and the default guest cap', async () => {
    mockFetch(routes());
    renderApp('/server?topic=demo');
    const select = await screen.findByRole('combobox', { name: 'Guests can listen to' });
    expect(within(select).getByRole('option', { name: 'Fiction' })).toBeInTheDocument();
    expect(screen.getByRole('textbox', { name: 'Most guests at once' })).toHaveAttribute(
      'placeholder',
      '200 (default)',
    );
  });
});
