import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { getToken, setToken } from '@/api/token';
import { mockFetch, type MockRoute } from '@/test/fetch-mock';
import {
  activity,
  admin,
  created,
  device,
  fictionGrant,
  invite,
  kidsShare,
  liveSession,
  member,
  sam,
  samDetail,
  serverInfo,
  users,
} from '@/test/fixtures';
import { renderApp } from '@/test/render-app';
import { signedInRoutes } from '@/test/routes';

function routes(over: Record<string, MockRoute> = {}) {
  return signedInRoutes({
    'GET /admin/users': { body: { users } },
    'GET /admin/users/2': { body: samDetail() },
    'GET /admin/users/1': {
      body: { user: admin, accessible_libraries: [], shares: [], auth_codes: [] },
    },
    'GET /admin/invites': { body: { invites: [invite()] } },
    'GET /admin/shares': { body: { shares: [kidsShare, fictionGrant] } },
    ...over,
  });
}

beforeEach(() => setToken('stored'));

describe('people', () => {
  it('shows everyone as a card with what they are listening to', async () => {
    mockFetch(
      routes({
        'GET /admin/sessions/live': { body: { sessions: [liveSession()] } },
        'GET /admin/devices': { body: { devices: [device()] } },
      }),
    );
    renderApp('/people');
    const card = await screen.findByRole('link', { name: /^sam/ });
    // sam has a live session on the book the stats fixture has in progress.
    expect(await within(card).findByText('Listening now')).toBeInTheDocument();
    expect(await within(card).findByText("Sam's iPhone")).toBeInTheDocument();
    expect(within(card).getAllByText('Project Hail Mary').length).toBeGreaterThan(0);
    expect(within(card).getByText('Paired devices only')).toBeInTheDocument();
    const me = screen.getByRole('link', { name: /^chris/ });
    expect(within(me).getByText('Admin')).toBeInTheDocument();
    expect(screen.getByText('2 accounts · 1 active this month')).toBeInTheDocument();
  });

  it('invites someone new: account, access and invite in one go, then the QR card', async () => {
    const calls = mockFetch(
      routes({
        'POST /admin/users': {
          status: 201,
          body: { ...sam, id: 9, username: 'Uncle Ray', last_seen_at: undefined },
        },
        'POST /admin/library-access': { status: 204 },
        'POST /admin/users/9/authcode': { status: 201, body: created },
      }),
    );
    renderApp('/people');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Invite someone' }));
    const dialog = await screen.findByRole('dialog', { name: 'Invite someone' });
    await user.type(within(dialog).getByLabelText('Their name'), 'Uncle Ray');
    await user.click(await within(dialog).findByRole('radio', { name: /All libraries/ }));
    await user.selectOptions(within(dialog).getByLabelText('Expires after'), '7');
    await user.click(within(dialog).getByRole('button', { name: 'Create invite' }));

    const ready = await screen.findByRole('dialog', { name: 'Invite ready for Uncle Ray' });
    expect(within(ready).getByRole('img', { name: /QR code/ })).toBeInTheDocument();
    // Named by the host the link uses (the public address), not the one this browser used.
    expect(within(ready).getByText('books.example.com')).toBeInTheDocument();
    expect(within(ready).getByLabelText('Invite link')).toHaveValue(created.invite_url);
    expect(within(ready).getByLabelText('Code')).toHaveValue('ABCD-1234');
    // The server's expiry for the new invite, as a date: it's a record.
    expect(within(ready).getByText(/^Expires [A-Z][a-z]{2} \d+, /)).toBeInTheDocument();

    expect(calls.find((c) => c.method === 'POST' && c.path === '/admin/users')?.body).toEqual({
      username: 'Uncle Ray',
      password: '',
      role: 'user',
    });
    const grants = calls.filter((c) => c.path === '/admin/library-access').map((c) => c.body);
    expect(grants).toEqual([
      { user_id: 9, library_id: 1 },
      { user_id: 9, library_id: 2 },
    ]);
    expect(calls.find((c) => c.path === '/admin/users/9/authcode')?.body).toEqual({
      label: 'invite',
      max_uses: 5,
      ttl_days: 7,
    });
    // The code never travels in a request URL (the QR is drawn in the browser).
    expect(
      calls.every((c) => !c.path.includes('ABCD') && !c.query.toString().includes('ABCD')),
    ).toBe(true);
  });

  it('explains a name that is already taken', async () => {
    mockFetch(
      routes({
        'POST /admin/users': {
          status: 409,
          body: { error: 'username already taken', code: 'username_taken' },
        },
      }),
    );
    renderApp('/people');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Invite someone' }));
    const dialog = await screen.findByRole('dialog', { name: 'Invite someone' });
    await user.type(within(dialog).getByLabelText('Their name'), 'sam');
    await user.click(within(dialog).getByRole('button', { name: 'Create invite' }));
    expect(await within(dialog).findByText(/Someone already has that name/)).toBeInTheDocument();
  });

  it('signs out an admin who was demoted mid-session', async () => {
    mockFetch(
      routes({
        'GET /admin/users': { status: 403, body: { error: 'forbidden' } },
        'GET /me': (req) => (req.headers.Authorization ? { body: admin } : { status: 401 }),
      }),
    );
    // The first /me (session check) says admin; once the 403 arrives the recheck says member.
    let checks = 0;
    const real = globalThis.fetch;
    vi.stubGlobal('fetch', (input: RequestInfo | URL, init?: RequestInit) => {
      if (String(input).endsWith('/api/v1/me') && ++checks > 1) {
        return Promise.resolve(new Response(JSON.stringify(member), { status: 200 }));
      }
      return real(input, init);
    });
    renderApp('/people');
    expect(await screen.findByRole('alert')).toHaveTextContent('not an administrator');
    expect(getToken()).toBeNull();
    vi.unstubAllGlobals();
  });
});

describe('a person', () => {
  it('shows what they can listen to and lets the admin change it', async () => {
    const calls = mockFetch(
      routes({
        'DELETE /admin/share-access': { status: 204 },
        'POST /admin/library-access': { status: 204 },
      }),
    );
    renderApp('/people/user/2?tab=access');
    expect(await screen.findByRole('heading', { level: 1, name: 'sam' })).toBeInTheDocument();
    // The sub bar turns into a breadcrumb back to People.
    expect(screen.getByRole('link', { name: 'Back to People' })).toBeInTheDocument();
    const access = screen.getByRole('region', { name: 'What sam can listen to' });
    expect(within(access).getByText('Cosy mysteries')).toBeInTheDocument();
    expect(within(access).getByText('Fiction › Agatha Christie')).toBeInTheDocument();

    const user = userEvent.setup();
    await user.click(
      within(access).getByRole('button', { name: 'Remove access to Cosy mysteries' }),
    );
    await waitFor(() =>
      expect(calls.find((c) => c.method === 'DELETE')?.body).toEqual({ user_id: 2, share_id: 7 }),
    );

    await user.click(within(access).getByRole('button', { name: 'Give access' }));
    const dialog = await screen.findByRole('dialog', { name: 'Give sam access' });
    await user.click(await within(dialog).findByRole('radio', { name: /Kids/ }));
    await user.click(within(dialog).getByRole('button', { name: 'Give access' }));
    await waitFor(() =>
      expect(calls.find((c) => c.path === '/admin/library-access')?.body).toEqual({
        user_id: 2,
        library_id: 2,
      }),
    );
  });

  it('makes a password-less member an admin by setting a password in the same step', async () => {
    const calls = mockFetch(
      routes({ 'PATCH /admin/users/2': { body: { ...sam, role: 'admin', has_password: true } } }),
    );
    renderApp('/people/user/2?tab=account');
    const user = userEvent.setup();
    await user.selectOptions(await screen.findByLabelText('Role'), 'admin');
    const dialog = await screen.findByRole('dialog', { name: 'Make sam an admin' });
    await user.type(within(dialog).getByLabelText('New password'), 'short');
    await user.type(within(dialog).getByLabelText('Type it again'), 'short');
    await user.click(within(dialog).getByRole('button', { name: 'Make admin' }));
    expect(await within(dialog).findByText('Use at least 8 characters.')).toBeInTheDocument();
    await user.clear(within(dialog).getByLabelText('New password'));
    await user.clear(within(dialog).getByLabelText('Type it again'));
    await user.type(within(dialog).getByLabelText('New password'), 'long-enough');
    await user.type(within(dialog).getByLabelText('Type it again'), 'long-enough');
    await user.click(within(dialog).getByRole('button', { name: 'Make admin' }));
    expect(await screen.findByText('sam is now an admin')).toBeInTheDocument();
    expect(calls.find((c) => c.method === 'PATCH')?.body).toEqual({
      password: 'long-enough',
      role: 'admin',
    });
  });

  it('pairs another device, and closing the invite card leaves no dialog behind', async () => {
    mockFetch(routes({ 'POST /admin/users/2/authcode': { status: 201, body: created } }));
    renderApp('/people/user/2?tab=invites');
    const user = userEvent.setup();
    for (const close of ['Done', 'Close']) {
      await user.click(await screen.findByRole('button', { name: 'Pair a device' }));
      const dialog = await screen.findByRole('dialog', { name: 'Pair a device for sam' });
      expect(within(dialog).getByText(/for sam's next phone/)).toBeInTheDocument();
      await user.click(within(dialog).getByRole('button', { name: 'Create invite' }));
      const ready = await screen.findByRole('dialog', { name: 'Invite ready for sam' });
      await user.click(within(ready).getByRole('button', { name: close }));
      // The form must not come back as a stray popup while the dialog closes.
      await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    }
  });

  it('cancelled while the invite is being made, it opens on the form next time', async () => {
    let answer = () => {};
    const answered = new Promise<void>((resolve) => (answer = resolve));
    mockFetch(
      routes({
        'POST /admin/users/2/authcode': async () => {
          await answered;
          return { status: 201, body: created };
        },
      }),
    );
    renderApp('/people/user/2?tab=invites');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Pair a device' }));
    const dialog = await screen.findByRole('dialog', { name: 'Pair a device for sam' });
    await user.click(within(dialog).getByRole('button', { name: 'Create invite' }));
    await user.click(within(dialog).getByRole('button', { name: 'Cancel' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    answer();
    // The invite that lands after Cancel neither brings the dialog back...
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    // ...nor greets the next open in place of the form.
    await user.click(screen.getByRole('button', { name: 'Pair a device' }));
    expect(
      await screen.findByRole('dialog', { name: 'Pair a device for sam' }),
    ).toBeInTheDocument();
  });

  it("doesn't let an admin demote, disable or delete themselves", async () => {
    mockFetch(routes());
    renderApp('/people/user/1?tab=account');
    expect(await screen.findByLabelText('Role')).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Disable account' })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Delete account' })).toBeDisabled();
  });

  it('deletes an account after its name is typed, offering to disable instead', async () => {
    const calls = mockFetch(routes({ 'DELETE /admin/users/2': { status: 204 } }));
    const { router } = renderApp('/people/user/2?tab=account');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Delete account' }));
    const dialog = await screen.findByRole('dialog', { name: 'Delete sam?' });
    expect(
      within(dialog).getByRole('button', { name: 'Disable the account instead' }),
    ).toBeInTheDocument();
    await user.type(within(dialog).getByRole('textbox'), 'sam');
    await user.click(within(dialog).getByRole('button', { name: 'Delete account' }));
    await waitFor(() => expect(router.state.location.pathname).toBe('/people'));
    expect(calls.some((c) => c.method === 'DELETE' && c.path === '/admin/users/2')).toBe(true);
  });

  it('disables an account', async () => {
    const calls = mockFetch(
      routes({ 'PATCH /admin/users/2': { body: { ...sam, disabled: true } } }),
    );
    renderApp('/people/user/2?tab=account');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Disable account' }));
    const dialog = await screen.findByRole('dialog', { name: 'Disable sam?' });
    await user.click(within(dialog).getByRole('button', { name: 'Disable account' }));
    expect(await screen.findByText('Disabled sam')).toBeInTheDocument();
    expect(calls.find((c) => c.method === 'PATCH')?.body).toEqual({ disabled: true });
  });

  it('revokes a saved recovery code', async () => {
    const calls = mockFetch(
      routes({
        'GET /admin/users/2': { body: samDetail({ user: { ...sam, has_recovery: true } }) },
        'DELETE /admin/users/2/recovery': { status: 204 },
      }),
    );
    renderApp('/people/user/2?tab=sign-in');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Revoke recovery code' }));
    const dialog = await screen.findByRole('dialog', { name: "Revoke sam's recovery code?" });
    await user.click(within(dialog).getByRole('button', { name: 'Revoke recovery code' }));
    await waitFor(() =>
      expect(calls.some((c) => c.method === 'DELETE' && c.path === '/admin/users/2/recovery')).toBe(
        true,
      ),
    );
  });

  it('counts their devices, and lists their API keys apart', async () => {
    mockFetch(
      routes({
        'GET /admin/devices': {
          body: {
            devices: [
              device(),
              device({ id: 3, kind: 'api', name: 'Home Assistant', client: null }),
            ],
          },
        },
      }),
    );
    renderApp('/people/user/2?tab=devices');
    // The count is devices only, as the person card's devices line counts them.
    expect(await screen.findByRole('tab', { name: /^Devices\s*1$/ })).toBeInTheDocument();
    expect(await screen.findByText("Sam's iPhone")).toBeInTheDocument();
    const keys = screen.getByRole('region', { name: 'API key' });
    expect(within(keys).getByText('Home Assistant')).toBeInTheDocument();
    expect(within(keys).queryByText("Sam's iPhone")).not.toBeInTheDocument();
  });

  it('404s an unknown person', async () => {
    mockFetch(
      routes({ 'GET /admin/users/99': { status: 404, body: { error: 'user not found' } } }),
    );
    renderApp('/people/user/99');
    expect(
      await screen.findByRole('heading', { name: "There's nothing here" }),
    ).toBeInTheDocument();
  });
});

describe('invites', () => {
  it('lists active invites and rotates one to show the fresh code', async () => {
    const calls = mockFetch(
      routes({
        'GET /admin/invites': {
          body: {
            invites: [
              invite(),
              invite({ id: 32, uses: 5, username: 'maya', user_id: 3 }), // used up
            ],
          },
        },
        'POST /admin/authcodes/31/rotate': { body: created },
        'GET /server': { body: { ...serverInfo, name: 'Tank' } },
      }),
    );
    renderApp('/people/invites');
    const table = await screen.findByRole('table');
    expect(within(table).getByText('sam')).toBeInTheDocument();
    expect(within(table).queryByText('maya')).not.toBeInTheDocument(); // used up: under All
    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: 'All · 2' }));
    expect(within(screen.getByRole('table')).getByText('maya')).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: "Rotate sam's invite" }));
    const ready = await screen.findByRole('dialog', { name: 'Invite ready for sam' });
    expect(within(ready).getByLabelText('Code')).toHaveValue('ABCD-1234');
    // A server the admin has named is called by that name, not its address.
    expect(within(ready).getByText('Tank')).toBeInTheDocument();
    expect(calls.some((c) => c.method === 'POST' && c.path === '/admin/authcodes/31/rotate')).toBe(
      true,
    );
  });

  it('revokes an invite after saying what happens to paired devices', async () => {
    const calls = mockFetch(routes({ 'DELETE /admin/authcodes/31': { status: 204 } }));
    renderApp('/people/invites');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: "Revoke sam's invite" }));
    const dialog = await screen.findByRole('dialog', { name: "Revoke sam's invite?" });
    expect(within(dialog).getByText(/Devices already paired stay signed in/)).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Revoke' }));
    await waitFor(() =>
      expect(calls.some((c) => c.method === 'DELETE' && c.path === '/admin/authcodes/31')).toBe(
        true,
      ),
    );
  });

  it('shows an empty state', async () => {
    mockFetch(routes({ 'GET /admin/invites': { body: { invites: [] } } }));
    renderApp('/people/invites');
    expect(await screen.findByRole('heading', { name: 'No invites yet' })).toBeInTheDocument();
  });
});

describe('devices', () => {
  it('lists every device and signs one out after confirming; the console itself is locked', async () => {
    const calls = mockFetch(
      routes({
        'GET /admin/devices': {
          body: {
            devices: [
              device(),
              device({
                id: 1,
                user_id: 1,
                username: 'chris',
                name: 'admin-web',
                current: true,
                client: { app: 'AudioSilo Admin', version: '', platform: 'web' },
              }),
              device({ id: 3, kind: 'api', name: 'Home Assistant', client: null }),
            ],
          },
        },
        'DELETE /admin/devices/7': { status: 204 },
      }),
    );
    renderApp('/people/devices');
    expect(await screen.findByText("Sam's iPhone")).toBeInTheDocument();
    expect(screen.getByText(/AudioSilo 1.4.2 · iOS · last seen/)).toBeInTheDocument();
    expect(screen.getByText(/Admin console/)).toBeInTheDocument();
    expect(screen.getByText('This device')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Sign out admin-web (chris)' })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Sign out Home Assistant (sam)' })).toHaveTextContent(
      'Revoke',
    );

    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: "Sign out Sam's iPhone (sam)" }));
    const dialog = await screen.findByRole('dialog', { name: "Sign out sam's Sam's iPhone?" });
    await user.click(within(dialog).getByRole('button', { name: 'Sign out' }));
    await waitFor(() =>
      expect(calls.some((c) => c.method === 'DELETE' && c.path === '/admin/devices/7')).toBe(true),
    );
  });

  it('has an empty state', async () => {
    mockFetch(routes());
    renderApp('/people/devices');
    expect(await screen.findByText('No devices yet')).toBeInTheDocument();
  });
});

describe("a person's listening", () => {
  const year = new Date().getFullYear();
  const progress = [
    {
      library_id: 1,
      path: 'Andy Weir/Project Hail Mary',
      position: 3000,
      duration: 6000,
      finished: false,
      playback_speed: 1,
      version: 3,
      device_id: 'iphone',
      updated_at: new Date().toISOString(),
      title: 'Project Hail Mary',
      author: 'Andy Weir',
      started_at: `${year}-01-05T10:00:00Z`,
      finished_at: null,
    },
    {
      library_id: 1,
      path: 'Martha Wells/All Systems Red',
      position: 100,
      duration: 100,
      finished: true,
      playback_speed: 1,
      version: 2,
      device_id: 'iphone',
      updated_at: `${year}-01-02T10:00:00Z`,
      title: 'All Systems Red',
      author: 'Martha Wells',
      started_at: `${year}-01-01T10:00:00Z`,
      finished_at: `${year}-01-02T10:00:00Z`,
    },
  ];

  function listeningRoutes(over: Record<string, MockRoute> = {}) {
    return routes({
      'GET /admin/users/2/progress': { body: { progress } },
      'GET /admin/sessions': { body: { sessions: [liveSession()], next_before: null } },
      'GET /admin/devices': { body: { devices: [device()] } },
      // sam's own days this year (5h: the fixture's per-day share for user 2).
      'GET /admin/listening': (req) => {
        expect(req.query.get('user_id')).toBe('2');
        return {
          body: {
            ...activity(),
            days: activity().days.map((d) => ({
              ...d,
              listened: d.by_user.find((u) => u.user_id === 2)!.listened,
              by_user: d.by_user.filter((u) => u.user_id === 2),
            })),
          },
        };
      },
      'PATCH /admin/libraries/1/progress': (req) => ({
        body: { progress: { ...progress[0], ...(req.body as object) } },
      }),
      ...over,
    });
  }

  it('shows their year, what they are in the middle of, what they finished and recent sessions', async () => {
    mockFetch(listeningRoutes());
    renderApp('/people/user/2');
    expect(await screen.findByText(`sam's listening year · ${year}`)).toBeInTheDocument();
    expect(await screen.findByText('5h')).toBeInTheDocument();
    const inProgress = await screen.findByRole('region', { name: 'In progress' });
    expect(within(inProgress).getByText('50% · saved now')).toBeInTheDocument();
    const finished = screen.getByRole('region', { name: 'Finished' });
    expect(within(finished).getByText(/Started .* · Finished/)).toBeInTheDocument();
    expect(await screen.findByRole('region', { name: 'Recent sessions' })).toBeInTheDocument();
    expect(screen.getByRole('tab', { name: /Devices/ })).toHaveTextContent('1');
  });

  it('marks a book finished, with Undo putting the position back', async () => {
    const calls = mockFetch(listeningRoutes());
    renderApp('/people/user/2');
    const user = userEvent.setup();
    const inProgress = await screen.findByRole('region', { name: 'In progress' });
    await user.click(
      await within(inProgress).findByRole('button', {
        name: 'Progress actions for sam on Project Hail Mary',
      }),
    );
    await user.click(await screen.findByRole('menuitem', { name: 'Mark as finished' }));
    await waitFor(() => {
      const patch = calls.find((c) => c.method === 'PATCH');
      expect(patch?.query.get('user_id')).toBe('2');
      expect(patch?.query.get('path')).toBe('Andy Weir/Project Hail Mary');
      expect(patch?.body).toEqual({ finished: true });
    });
    await user.click(await screen.findByRole('button', { name: 'Undo' }));
    await waitFor(() =>
      expect(calls.filter((c) => c.method === 'PATCH').at(-1)?.body).toEqual({
        finished: false,
        position: 3000,
      }),
    );
  });

  it('edits the dates, sending only what changed', async () => {
    const calls = mockFetch(listeningRoutes());
    renderApp('/people/user/2');
    const user = userEvent.setup();
    const finished = await screen.findByRole('region', { name: 'Finished' });
    await user.click(
      await within(finished).findByRole('button', {
        name: 'Progress actions for sam on All Systems Red',
      }),
    );
    await user.click(await screen.findByRole('menuitem', { name: 'Edit dates…' }));
    const dialog = await screen.findByRole('dialog');
    const started = within(dialog).getByLabelText('Started on');
    expect(started).toHaveValue(`${year}-01-01`);
    await user.clear(started);
    await user.click(within(dialog).getByRole('button', { name: 'Save dates' }));
    await waitFor(() =>
      expect(calls.find((c) => c.method === 'PATCH')?.body).toEqual({ started_at: null }),
    );
  });

  it("says when the person can't see the book", async () => {
    mockFetch(
      listeningRoutes({
        'PATCH /admin/libraries/1/progress': {
          status: 409,
          body: { error: 'no access', code: 'no_access' },
        },
      }),
    );
    renderApp('/people/user/2');
    const user = userEvent.setup();
    const inProgress = await screen.findByRole('region', { name: 'In progress' });
    await user.click(
      await within(inProgress).findByRole('button', {
        name: 'Progress actions for sam on Project Hail Mary',
      }),
    );
    await user.click(await screen.findByRole('menuitem', { name: 'Mark as finished' }));
    expect(
      await screen.findByText("This person can't see that book. Give them access first."),
    ).toBeInTheDocument();
  });
});
