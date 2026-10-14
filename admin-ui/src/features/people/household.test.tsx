import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { setToken } from '@/api/token';
import type { AdminMember, AdminUser, UserDetail } from '@/api/types';
import { mockFetch, type MockRoute } from '@/test/fetch-mock';
import { admin, invite, libraries, sam, samDetail, settingsWith } from '@/test/fixtures';
import { renderApp } from '@/test/render-app';
import { signedInRoutes } from '@/test/routes';

// Household members in the console: People groups them under their owner, a
// person's page manages them, and deleting an owner asks which to keep.

const adam: AdminUser = { ...sam, id: 5, username: 'adam', members_count: 2 };
const evie: AdminUser = {
  ...sam,
  id: 6,
  username: 'evie',
  display_name: 'Evie',
  managed_by: { id: 5, username: 'adam', display_name: '' },
  member_kind: 'kid',
  member_access: 'collections',
  can_add_members: false,
};
const ellis: AdminUser = {
  ...evie,
  id: 7,
  username: 'ellis',
  display_name: 'Ellis',
  member_kind: 'grownup',
  member_access: 'all',
};
// The server's order (by name): the members come before their owner.
const everyone = [admin, ellis, evie, sam, adam];

const adamMembers: AdminMember[] = [
  {
    id: 6,
    username: 'evie',
    name: 'Evie',
    kind: 'kid',
    access: 'collections',
    created_at: '2026-10-01T09:00:00.000Z',
    devices: 1,
    disabled: false,
  },
  {
    id: 7,
    username: 'ellis',
    name: 'Ellis',
    kind: 'grownup',
    access: 'all',
    created_at: '2026-10-02T09:00:00.000Z',
    devices: 0,
    disabled: false,
  },
];

function detail(user: AdminUser, over: Partial<UserDetail> = {}): UserDetail {
  return samDetail({ user, shares: [], auth_codes: [], ...over });
}

function routes(over: Record<string, MockRoute> = {}) {
  return signedInRoutes({
    'GET /admin/users': { body: { users: everyone } },
    'GET /admin/users/1': { body: detail(admin) },
    'GET /admin/users/2': { body: detail(sam) },
    'GET /admin/users/5': {
      body: detail(adam, { members: adamMembers, accessible_libraries: [libraries()[1]] }),
    },
    'GET /admin/users/6': {
      body: detail(evie, {
        member_collections: [
          { id: 4, name: 'Bedtime', items: 12 },
          { id: 9, name: 'Car journeys', items: 1 },
        ],
      }),
    },
    'GET /admin/users/7': {
      body: detail(ellis, { owner_libraries: [libraries()[1]], owner_is_admin: false }),
    },
    'GET /admin/invites': { body: { invites: [] } },
    ...over,
  });
}

beforeEach(() => setToken('stored'));

describe('household members on People', () => {
  it('show a household as one block, each member with who manages them and Kid or Grown-up', async () => {
    mockFetch(routes());
    renderApp('/people');
    await screen.findByRole('link', { name: /^adam/ });
    const cardsIn = (el: HTMLElement) =>
      within(el)
        .getAllByRole('link')
        .map((a) => a.getAttribute('href') ?? '')
        .filter((h) => h.startsWith('/admin/people/user/'))
        .map((h) => Number(h.split('/').pop()));
    expect(cardsIn(document.body)).toEqual([1, 2, 5, 7, 6]);
    // The owner leads their household's block; ordinary people stay outside it.
    const household = screen.getByRole('region', { name: "adam's household" });
    expect(cardsIn(household)).toEqual([5, 7, 6]);
    expect(screen.getAllByRole('region', { name: /household/ })).toHaveLength(1);

    const card = screen.getByRole('link', { name: /^evie/ });
    expect(within(card).getByText('Managed by adam')).toBeInTheDocument();
    expect(within(card).getByText('Kid')).toBeInTheDocument();
    expect(
      within(screen.getByRole('link', { name: /^ellis/ })).getByText('Grown-up'),
    ).toBeInTheDocument();
    const owner = screen.getByRole('link', { name: /^adam/ });
    expect(within(owner).getByText('2 household members')).toBeInTheDocument();
    expect(within(owner).queryByText(/Managed by/)).not.toBeInTheDocument();
  });

  it("still shows a member whose owner isn't listed", async () => {
    mockFetch(routes({ 'GET /admin/users': { body: { users: [admin, evie] } } }));
    renderApp('/people');
    const card = await screen.findByRole('link', { name: /^evie/ });
    expect(within(card).getByText('Managed by adam')).toBeInTheDocument();
  });
});

describe("an owner's page", () => {
  it('lists their members and switches off adding more', async () => {
    const calls = mockFetch(
      routes({ 'PATCH /admin/users/5': { body: { ...adam, can_add_members: false } } }),
    );
    renderApp('/people/user/5?tab=account');
    const household = await screen.findByRole('region', { name: 'Household' });
    const members = within(household).getByRole('region', { name: "adam's household" });
    expect(within(members).getByRole('link', { name: /Evie/ })).toHaveAttribute(
      'href',
      '/admin/people/user/6',
    );
    expect(within(members).getByRole('link', { name: /Ellis/ })).toHaveTextContent('Grown-up');
    // adam manages members, so can't join someone else's household.
    expect(within(household).getByRole('button', { name: 'Assign to someone' })).toBeDisabled();

    const user = userEvent.setup();
    const toggle = within(household).getByRole('switch', { name: 'Can add household members' });
    expect(toggle).toBeChecked();
    // The cap comes with the server's settings.
    await waitFor(() =>
      expect(toggle).toHaveAccessibleDescription(
        'adam can add kids and grown-ups from the player, up to 6.',
      ),
    );
    await user.click(toggle);
    await waitFor(() =>
      expect(calls.find((c) => c.method === 'PATCH')?.body).toEqual({ can_add_members: false }),
    );
    expect(
      await screen.findByText("adam can't add household members any more"),
    ).toBeInTheDocument();
  });

  it("says so when they can't add members, and that existing members keep working", async () => {
    const off = { ...adam, can_add_members: false };
    mockFetch(routes({ 'GET /admin/users/5': { body: detail(off, { members: adamMembers }) } }));
    renderApp('/people/user/5?tab=account');
    const toggle = await screen.findByRole('switch', { name: 'Can add household members' });
    expect(toggle).not.toBeChecked();
    expect(toggle).toHaveAccessibleDescription(
      "adam can't add household members. Members already added keep working.",
    );
  });

  it('points to Settings when household members are off for the server', async () => {
    mockFetch(
      routes({
        'GET /admin/settings': {
          body: settingsWith({
            household: {
              members_enabled: false,
              max_members: 6,
              max_members_default: 6,
              max_members_min: 1,
              max_members_max: 50,
            },
          }),
        },
      }),
    );
    renderApp('/people/user/5?tab=account');
    expect(
      await screen.findByText('Household members are off for the whole server.'),
    ).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Change it in Settings' })).toHaveAttribute(
      'href',
      '/admin/server?topic=household',
    );
  });

  it('deletes the owner with the ticked members and keeps the unticked ones', async () => {
    const calls = mockFetch(routes({ 'DELETE /admin/users/5': { status: 204 } }));
    const { router } = renderApp('/people/user/5?tab=account');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Delete account' }));
    const dialog = await screen.findByRole('dialog', { name: 'Delete adam?' });
    const group = within(dialog).getByRole('group', { name: 'Their 2 household members' });
    const evieBox = within(group).getByRole('checkbox', { name: /Delete Evie too/ });
    const ellisBox = within(group).getByRole('checkbox', { name: /Delete Ellis too/ });
    expect(evieBox).toBeChecked();
    expect(ellisBox).toBeChecked();
    await user.click(ellisBox);
    expect(ellisBox).not.toBeChecked();
    await user.type(within(dialog).getByRole('textbox'), 'adam');
    await user.click(within(dialog).getByRole('button', { name: 'Delete account' }));
    await waitFor(() => expect(router.state.location.pathname).toBe('/people'));
    const del = calls.find((c) => c.method === 'DELETE');
    expect(del?.path).toBe('/admin/users/5');
    expect(del?.query.get('keep_members')).toBe('7');
    expect(
      await screen.findByText(
        '1 household member was deleted too. 1 household member is now an independent account.',
      ),
    ).toBeInTheDocument();
  });

  it('sends no keep list when every member goes too', async () => {
    const calls = mockFetch(routes({ 'DELETE /admin/users/5': { status: 204 } }));
    renderApp('/people/user/5?tab=account');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Delete account' }));
    const dialog = await screen.findByRole('dialog', { name: 'Delete adam?' });
    await user.type(within(dialog).getByRole('textbox'), 'adam');
    await user.click(within(dialog).getByRole('button', { name: 'Delete account' }));
    await waitFor(() => expect(calls.some((c) => c.method === 'DELETE')).toBe(true));
    expect(calls.find((c) => c.method === 'DELETE')?.query.has('keep_members')).toBe(false);
  });

  it('asks nothing about members for someone without any', async () => {
    mockFetch(routes());
    renderApp('/people/user/2?tab=account');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Delete account' }));
    const dialog = await screen.findByRole('dialog', { name: 'Delete sam?' });
    expect(within(dialog).queryByRole('checkbox')).not.toBeInTheDocument();
  });
});

describe("a member's page", () => {
  it('shows who manages them and makes them independent after confirming', async () => {
    const calls = mockFetch(
      routes({
        'PATCH /admin/users/6': {
          body: { ...evie, managed_by: null, member_kind: null, member_access: null },
        },
      }),
    );
    renderApp('/people/user/6?tab=account');
    const household = await screen.findByRole('region', { name: 'Household' });
    expect(within(household).getByRole('link', { name: 'adam' })).toHaveAttribute(
      'href',
      '/admin/people/user/5',
    );
    expect(within(household).queryByRole('switch')).not.toBeInTheDocument();
    // The header carries the same chips as the People card.
    expect(screen.getByText('Managed by adam')).toBeInTheDocument();

    const user = userEvent.setup();
    await user.click(within(household).getByRole('button', { name: 'Make independent' }));
    const dialog = await screen.findByRole('dialog', { name: 'Make evie independent?' });
    // A kid has no password: the dialog says how they sign in from here.
    expect(within(dialog).getByText(/has no password/)).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Make independent' }));
    await waitFor(() =>
      expect(calls.find((c) => c.method === 'PATCH')?.body).toEqual({ managed_by: null }),
    );
    expect(await screen.findByText('evie is now an independent account')).toBeInTheDocument();
  });

  it("can't be made an admin, and says why", async () => {
    mockFetch(routes());
    renderApp('/people/user/6?tab=account');
    const role = await screen.findByLabelText('Role');
    expect(within(role).getByRole('option', { name: 'Admin' })).toBeDisabled();
    expect(
      screen.getByText(
        "evie is in adam's household, so they can't be an admin. Make them independent first.",
      ),
    ).toBeInTheDocument();
  });

  it('offers no password for a kid, and says how they sign in instead', async () => {
    mockFetch(routes());
    renderApp('/people/user/6?tab=sign-in');
    const card = await screen.findByRole('region', { name: 'Password' });
    expect(
      within(card).getByText(
        'Kids sign in without a password: adam signs them in, or uses “Set up their own device” in the player.',
      ),
    ).toBeInTheDocument();
    expect(within(card).queryByRole('button')).not.toBeInTheDocument();
  });

  it('offers a grown-up member a password', async () => {
    mockFetch(routes());
    renderApp('/people/user/7?tab=sign-in');
    const card = await screen.findByRole('region', { name: 'Password' });
    expect(within(card).getByRole('button', { name: 'Set a password' })).toBeInTheDocument();
    expect(within(card).queryByText(/Kids sign in without a password/)).not.toBeInTheDocument();
  });

  it('lists the collections their owner shares with them, read-only, beside direct grants', async () => {
    mockFetch(routes());
    renderApp('/people/user/6?tab=access');
    const shared = await screen.findByRole('region', {
      name: 'Collections adam shares with them',
    });
    expect(within(shared).getByText('Bedtime')).toBeInTheDocument();
    expect(within(shared).getByText('12 books')).toBeInTheDocument();
    expect(within(shared).getByText('1 book')).toBeInTheDocument();
    expect(within(shared).queryByRole('button')).not.toBeInTheDocument();
    const direct = screen.getByRole('region', { name: 'Access given here' });
    expect(within(direct).getByText('Nothing given directly.')).toBeInTheDocument();
  });

  it("lists their owner's libraries when they get everything the owner can listen to", async () => {
    mockFetch(routes());
    renderApp('/people/user/7?tab=access');
    const through = await screen.findByRole('region', { name: 'Everything adam can listen to' });
    expect(await within(through).findByText('Kids')).toBeInTheDocument();
    expect(within(through).queryByText('Fiction')).not.toBeInTheDocument();
  });

  it('says no collection is shared yet', async () => {
    mockFetch(routes({ 'GET /admin/users/6': { body: detail(evie) } }));
    renderApp('/people/user/6?tab=access');
    expect(
      await screen.findByText("adam hasn't shared a collection with evie yet."),
    ).toBeInTheDocument();
  });
});

describe('assigning someone to a household', () => {
  it('sends the owner, kind and access together, warning what a kid loses', async () => {
    const calls = mockFetch(routes({ 'PATCH /admin/users/2': { body: sam } }));
    renderApp('/people/user/2?tab=account');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Assign to someone' }));
    const dialog = await screen.findByRole('dialog', { name: 'Assign sam to someone' });
    const owner = within(dialog).getByLabelText('Whose household');
    // Members and sam themself can't manage anyone.
    expect(
      within(owner)
        .getAllByRole('option')
        .map((o) => o.textContent),
    ).toEqual(['Choose a person', 'chris', 'adam']);
    expect(within(dialog).getByRole('button', { name: 'Assign to someone' })).toBeDisabled();
    // Nobody picked yet: the access copy has its own wording, not a name's place-holder.
    expect(
      within(dialog).getByRole('radio', { name: /^Everything the person you pick can listen to/ }),
    ).toBeInTheDocument();
    expect(
      within(dialog).getByText('Follows the access of the person you pick, as it changes.'),
    ).toBeInTheDocument();
    await user.selectOptions(owner, 'adam');
    // A grown-up starts on everything their owner can listen to.
    expect(within(dialog).getByRole('radio', { name: /^Grown-up/ })).toBeChecked();
    expect(
      within(dialog).getByRole('radio', { name: /^Everything adam can listen to/ }),
    ).toBeChecked();
    await user.click(within(dialog).getByRole('radio', { name: /^Kid/ }));
    // A kid starts on shared collections, and loses their password.
    expect(
      within(dialog).getByRole('radio', { name: /^Only collections adam shares/ }),
    ).toBeChecked();
    expect(within(dialog).getByText(/removes their password/)).toBeInTheDocument();
    expect(
      within(dialog).getByText(/gets books from adam only through the collections adam shares/),
    ).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Assign to someone' }));
    await waitFor(() =>
      expect(calls.find((c) => c.method === 'PATCH')?.body).toEqual({
        managed_by: 5,
        member_kind: 'kid',
        member_access: 'collections',
      }),
    );
    expect(await screen.findByText("sam is now in adam's household")).toBeInTheDocument();
  });

  it('keeps an access picked by hand when the kind changes', async () => {
    const calls = mockFetch(routes({ 'PATCH /admin/users/2': { body: sam } }));
    renderApp('/people/user/2?tab=account');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Assign to someone' }));
    const dialog = await screen.findByRole('dialog', { name: 'Assign sam to someone' });
    await user.selectOptions(within(dialog).getByLabelText('Whose household'), 'chris');
    await user.click(within(dialog).getByRole('radio', { name: /^Only collections chris/ }));
    await user.click(within(dialog).getByRole('radio', { name: /^Kid/ }));
    await user.click(within(dialog).getByRole('radio', { name: /^Grown-up/ }));
    await user.click(within(dialog).getByRole('button', { name: 'Assign to someone' }));
    await waitFor(() =>
      expect(calls.find((c) => c.method === 'PATCH')?.body).toEqual({
        managed_by: 1,
        member_kind: 'grownup',
        member_access: 'collections',
      }),
    );
  });

  it("moves a member to someone new, and words the server's refusal", async () => {
    mockFetch(
      routes({
        'PATCH /admin/users/6': {
          status: 409,
          body: { error: 'owner is a member', code: 'owner_is_member' },
        },
      }),
    );
    renderApp('/people/user/6?tab=account');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Assign to someone' }));
    const dialog = await screen.findByRole('dialog', { name: 'Assign evie to someone' });
    const owner = within(dialog).getByLabelText('Whose household');
    // Their own owner isn't offered; their kind and access are where they are now.
    expect(within(owner).queryByRole('option', { name: 'adam' })).not.toBeInTheDocument();
    expect(within(dialog).getByRole('radio', { name: /^Kid/ })).toBeChecked();
    expect(within(dialog).queryByText(/removes their password/)).not.toBeInTheDocument();
    await user.selectOptions(owner, 'sam');
    await user.click(within(dialog).getByRole('button', { name: 'Assign to someone' }));
    expect(
      await within(dialog).findByText(
        "That person is a household member, so they can't manage anyone. Pick someone else.",
      ),
    ).toBeInTheDocument();
  });

  it("isn't offered for an admin", async () => {
    mockFetch(routes());
    renderApp('/people/user/1?tab=account');
    expect(await screen.findByRole('region', { name: 'Household' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Assign to someone' })).not.toBeInTheDocument();
  });
});

describe("a kid's devices", () => {
  const kidLine =
    "Kids don't use invites: adam signs them in, or uses “Set up their own device” in the player.";
  // An expired invite kept as history from before the switch to kid.
  const expired = invite({
    user_id: 6,
    username: 'evie',
    expires_at: new Date(Date.now() - 86400_000).toISOString(),
  });

  it('offers no pairing for a kid, and says who sets up their devices', async () => {
    mockFetch(routes());
    renderApp('/people/user/6?tab=devices');
    expect(await screen.findByText('No devices yet')).toBeInTheDocument();
    expect(screen.getByText(kidLine)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Pair a device' })).not.toBeInTheDocument();
  });

  it('offers no invite for a kid', async () => {
    mockFetch(routes());
    renderApp('/people/user/6?tab=invites');
    expect(await screen.findByText('No invites yet')).toBeInTheDocument();
    expect(screen.getByText(kidLine)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Pair a device' })).not.toBeInTheDocument();
  });

  it("offers no Rotate of a kid's invite kept as history", async () => {
    mockFetch(routes({ 'GET /admin/users/6': { body: detail(evie, { auth_codes: [expired] }) } }));
    renderApp('/people/user/6?tab=invites');
    expect(await screen.findByRole('button', { name: "Revoke evie's invite" })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /^Rotate/ })).not.toBeInTheDocument();
  });

  it('still offers a grown-up member pairing and Rotate', async () => {
    mockFetch(
      routes({
        'GET /admin/users/7': {
          body: detail(ellis, { auth_codes: [expired] }),
        },
      }),
    );
    renderApp('/people/user/7?tab=invites');
    expect(
      await screen.findByRole('button', { name: "Rotate ellis's invite" }),
    ).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Pair a device' })).toBeInTheDocument();
    expect(screen.queryByText(kidLine)).not.toBeInTheDocument();
  });
});
