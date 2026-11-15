import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { setToken } from '@/api/token';
import type { AdminUser, OIDCIdentity, OIDCPendingSignIn, OIDCStatus } from '@/api/types';
import { mockFetch, type MockRoute } from '@/test/fetch-mock';
import { admin, sam, samDetail } from '@/test/fixtures';
import { renderApp } from '@/test/render-app';
import { signedInRoutes } from '@/test/routes';

// People > Waiting sign-ins (server#235): single sign-ons no account took,
// linked, given an account or dismissed by an admin; and a person's linked
// sign-on on their page, with Unlink.

const ISSUER = 'https://sso.example.org/realms/home';

const bobRow: OIDCPendingSignIn = {
  id: 4,
  issuer: ISSUER,
  subject: 'sub-bob',
  email: 'bob@example.org',
  email_verified: false,
  username: 'bob',
  name: 'Bob Smith',
  groups: ['listeners', 'family', 'kids', 'admins'],
  reason: 'unverified_email',
  first_seen_at: new Date(Date.now() - 2 * 86400_000).toISOString(),
  last_seen_at: new Date(Date.now() - 3600_000).toISOString(),
  attempts: 3,
};

const bob: AdminUser = { ...sam, id: 5, username: 'bob', email: 'bob@example.org' };
const kid: AdminUser = {
  ...sam,
  id: 6,
  username: 'evie',
  managed_by: { id: 1, username: 'chris', display_name: '' },
  member_kind: 'kid',
  member_access: 'all',
};

function routes(over: Record<string, MockRoute> = {}) {
  return signedInRoutes({
    'GET /admin/users': { body: { users: [admin, sam, bob, kid] } },
    'GET /admin/oidc/pending': {
      body: { pending: [bobRow], linked_user_ids: { [ISSUER]: [2] } },
    },
    ...over,
  });
}

beforeEach(() => setToken('stored'));

describe('waiting sign-ins', () => {
  it('lists each with its email, reason, groups and attempts, and counts them on the tab', async () => {
    mockFetch(routes());
    renderApp('/people');
    const card = await screen.findByRole('region', { name: /Waiting sign-ins/ });
    expect(within(card).getByText('Bob Smith')).toBeInTheDocument();
    expect(within(card).getByText('bob@example.org')).toBeInTheDocument();
    expect(within(card).getByText('Not verified')).toBeInTheDocument();
    expect(
      within(card).getByText(
        "Their email matches an account, but the provider hasn't verified it.",
      ),
    ).toBeInTheDocument();
    expect(
      within(card).getByText('Groups: listeners, family, kids and 1 more'),
    ).toBeInTheDocument();
    expect(within(card).getByText(/· 3 attempts$/)).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'People, 1 waiting sign-in' })).toBeInTheDocument();
  });

  it('is not shown while nobody waits', async () => {
    mockFetch(signedInRoutes({ 'GET /admin/users': { body: { users: [admin, sam] } } }));
    renderApp('/people');
    await screen.findByText('sam');
    expect(screen.queryByRole('region', { name: /Waiting sign-ins/ })).toBeNull();
    expect(screen.queryByRole('link', { name: /waiting sign-in/ })).toBeNull();
  });

  it('links to an account the admin picks: no member or already linked one offered', async () => {
    const calls = mockFetch(routes({ 'POST /admin/oidc/pending/4/link': { body: bob } }));
    renderApp('/people');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Link' }));
    const dialog = await screen.findByRole('dialog', { name: 'Link Bob Smith to an account' });
    const select = within(dialog).getByRole('combobox', { name: 'Account' });
    const options = within(select)
      .getAllByRole('option')
      .map((o) => o.textContent);
    expect(options).toEqual(['Choose an account', 'chris (admin)', 'bob']);
    // Bob's email is not verified: the account with that address is not picked for the admin.
    expect(select).toHaveValue('');
    expect(within(dialog).getByRole('button', { name: 'Link' })).toBeDisabled();
    await user.selectOptions(select, '5');
    await user.click(within(dialog).getByRole('button', { name: 'Link' }));
    expect(await screen.findByText('Linked to bob')).toBeInTheDocument();
    expect(calls.find((c) => c.path === '/admin/oidc/pending/4/link')?.body).toEqual({
      user_id: 5,
    });
  });

  it('suggests the account with the sign-in’s email once the provider verified it', async () => {
    // What waits with a verified email that one account has: an admin's, never linked on its own.
    mockFetch(
      routes({
        'GET /admin/users': { body: { users: [admin, sam, { ...bob, role: 'admin' }, kid] } },
        'GET /admin/oidc/pending': {
          body: {
            pending: [{ ...bobRow, email_verified: true, reason: 'admin_account' }],
            linked_user_ids: {},
          },
        },
      }),
    );
    renderApp('/people');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Link' }));
    const dialog = await screen.findByRole('dialog', { name: 'Link Bob Smith to an account' });
    await waitFor(() =>
      expect(within(dialog).getByRole('combobox', { name: 'Account' })).toHaveValue('5'),
    );
  });

  it('says why a link was refused', async () => {
    mockFetch(
      routes({
        'POST /admin/oidc/pending/4/link': {
          status: 409,
          body: { error: 'has one', code: 'user_has_identity' },
        },
      }),
    );
    renderApp('/people');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Link' }));
    const dialog = await screen.findByRole('dialog', { name: 'Link Bob Smith to an account' });
    await user.selectOptions(within(dialog).getByRole('combobox', { name: 'Account' }), '5');
    await user.click(within(dialog).getByRole('button', { name: 'Link' }));
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(
      'That account already has a single sign-on from this provider.',
    );
  });

  it('keeps a refused dialog open when its row went meanwhile and the list empties', async () => {
    let rows = [bobRow];
    mockFetch(
      routes({
        'GET /admin/oidc/pending': () => ({ body: { pending: rows, linked_user_ids: {} } }),
        'POST /admin/oidc/pending/4/link': () => {
          rows = [];
          return { status: 404, body: { error: 'the sign-in is gone', code: 'not_found' } };
        },
      }),
    );
    renderApp('/people');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Link' }));
    const dialog = await screen.findByRole('dialog', { name: 'Link Bob Smith to an account' });
    await user.selectOptions(within(dialog).getByRole('combobox', { name: 'Account' }), '5');
    await user.click(within(dialog).getByRole('button', { name: 'Link' }));
    expect(await within(dialog).findByRole('alert')).toHaveTextContent('the sign-in is gone');
    await waitFor(() =>
      expect(screen.queryByRole('region', { name: /Waiting sign-ins/ })).toBeNull(),
    );
    expect(
      screen.getByRole('dialog', { name: 'Link Bob Smith to an account' }),
    ).toBeInTheDocument();
  });

  it('drops a picked account from the choice once a refusal says it is linked', async () => {
    let linked: Record<string, number[]> = {};
    mockFetch(
      routes({
        'GET /admin/oidc/pending': () => ({
          body: { pending: [bobRow], linked_user_ids: linked },
        }),
        'POST /admin/oidc/pending/4/link': () => {
          linked = { [ISSUER]: [2] };
          return { status: 409, body: { error: 'has one', code: 'user_has_identity' } };
        },
      }),
    );
    renderApp('/people');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Link' }));
    const dialog = await screen.findByRole('dialog', { name: 'Link Bob Smith to an account' });
    await user.selectOptions(within(dialog).getByRole('combobox', { name: 'Account' }), '2');
    await user.click(within(dialog).getByRole('button', { name: 'Link' }));
    await within(dialog).findByRole('alert');
    await waitFor(() =>
      expect(within(dialog).getByRole('combobox', { name: 'Account' })).toHaveValue(''),
    );
    expect(within(dialog).getByRole('button', { name: 'Link' })).toBeDisabled();
  });

  it('says a refusal that is not about the username apart from the field', async () => {
    mockFetch(
      routes({
        'POST /admin/oidc/pending/4/create': {
          status: 409,
          body: { error: 'linked meanwhile', code: 'identity_linked' },
        },
      }),
    );
    renderApp('/people');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Create account' }));
    const dialog = await screen.findByRole('dialog', { name: 'Make an account for Bob Smith' });
    await user.click(within(dialog).getByRole('button', { name: 'Create account' }));
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(
      'This sign-in was linked to an account meanwhile.',
    );
    expect(within(dialog).getByRole('textbox', { name: 'Username' })).not.toHaveAttribute(
      'aria-invalid',
    );
  });

  it('creates an account with the username typed, prefilled from the sign-in', async () => {
    const calls = mockFetch(
      routes({
        'POST /admin/oidc/pending/4/create': {
          status: 201,
          body: { ...bob, username: 'bobby' },
        },
      }),
    );
    renderApp('/people');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Create account' }));
    const dialog = await screen.findByRole('dialog', { name: 'Make an account for Bob Smith' });
    const field = within(dialog).getByRole('textbox', { name: 'Username' });
    expect(field).toHaveValue('bob');
    await user.clear(field);
    await user.type(field, ' bobby ');
    await user.click(within(dialog).getByRole('button', { name: 'Create account' }));
    expect(await screen.findByText('Account bobby made')).toBeInTheDocument();
    expect(calls.find((c) => c.path === '/admin/oidc/pending/4/create')?.body).toEqual({
      username: 'bobby',
    });
  });

  it('says a username is taken under the field', async () => {
    mockFetch(
      routes({
        'POST /admin/oidc/pending/4/create': {
          status: 409,
          body: { error: 'taken', code: 'username_taken' },
        },
      }),
    );
    renderApp('/people');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Create account' }));
    const dialog = await screen.findByRole('dialog', { name: 'Make an account for Bob Smith' });
    await user.click(within(dialog).getByRole('button', { name: 'Create account' }));
    expect(await within(dialog).findByRole('alert')).toBeInTheDocument();
    expect(within(dialog).getByRole('textbox', { name: 'Username' })).toHaveAttribute(
      'aria-invalid',
      'true',
    );
  });

  it('dismisses after a confirm', async () => {
    let gone = false;
    const calls = mockFetch(
      routes({
        'GET /admin/oidc/pending': () => ({
          body: { pending: gone ? [] : [bobRow], linked_user_ids: {} },
        }),
        'DELETE /admin/oidc/pending/4': () => {
          gone = true;
          return { status: 204 };
        },
      }),
    );
    renderApp('/people');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: "Dismiss Bob Smith's sign-in" }));
    const dialog = await screen.findByRole('dialog', { name: "Dismiss Bob Smith's sign-in?" });
    expect(calls.some((c) => c.method === 'DELETE')).toBe(false);
    await user.click(within(dialog).getByRole('button', { name: 'Dismiss' }));
    expect(await screen.findByText('Sign-in dismissed')).toBeInTheDocument();
    await waitFor(() =>
      expect(screen.queryByRole('region', { name: /Waiting sign-ins/ })).toBeNull(),
    );
  });

  it('shows a list that failed to load', async () => {
    mockFetch(routes({ 'GET /admin/oidc/pending': { status: 500, body: { error: 'boom' } } }));
    renderApp('/people');
    expect(await screen.findByText("Couldn't load the waiting sign-ins")).toBeInTheDocument();
  });
});

const identity: OIDCIdentity = {
  id: 9,
  issuer: ISSUER,
  subject: 'sub-sam',
  email: 'sam@example.org',
  username: 'sam',
  link_method: 'admin',
  linked_by: { id: 1, username: 'chris' },
  linked_at: new Date(Date.now() - 86400_000).toISOString(),
  last_sign_in_at: null,
};

const status: OIDCStatus = {
  enabled: true,
  configured: true,
  name: 'Keycloak',
  issuer: ISSUER,
  client_id: 'audiosilo',
  client_secret_set: true,
  redirect_uri: 'https://books.example.org/api/v1/auth/oidc/callback',
  scopes: ['openid'],
  username_claim: 'preferred_username',
  groups_claim: 'groups',
};

function personRoutes(user: AdminUser, over: Record<string, MockRoute> = {}) {
  return signedInRoutes({
    'GET /admin/users/2': { body: samDetail({ user, identities: [identity] }) },
    'GET /admin/oidc/status': { body: status },
    ...over,
  });
}

describe('a person’s single sign-on', () => {
  it('shows who it is, how it was linked, and unlinks after a confirm', async () => {
    const calls = mockFetch(
      personRoutes(sam, { 'DELETE /admin/users/2/identities/9': { status: 204 } }),
    );
    renderApp('/people/user/2?tab=sign-in');
    const card = await screen.findByRole('region', { name: 'Signed in with Keycloak' });
    expect(within(card).getByText('sam@example.org')).toBeInTheDocument();
    expect(within(card).getByText(/^By chris · /)).toBeInTheDocument();
    expect(within(card).getByText('Not yet')).toBeInTheDocument();
    const user = userEvent.setup();
    await user.click(within(card).getByRole('button', { name: 'Unlink' }));
    const dialog = await screen.findByRole('dialog', { name: 'Unlink Keycloak from sam?' });
    await user.click(within(dialog).getByRole('button', { name: 'Unlink' }));
    expect(await screen.findByText('Keycloak unlinked')).toBeInTheDocument();
    expect(
      calls.some((c) => c.method === 'DELETE' && c.path === '/admin/users/2/identities/9'),
    ).toBe(true);
  });

  it('says a password-less admin needs a password first', async () => {
    mockFetch(
      personRoutes(
        { ...sam, role: 'admin' },
        {
          'DELETE /admin/users/2/identities/9': {
            status: 409,
            body: { error: 'no password', code: 'admin_needs_password' },
          },
        },
      ),
    );
    renderApp('/people/user/2?tab=sign-in');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Unlink' }));
    const dialog = await screen.findByRole('dialog', { name: 'Unlink Keycloak from sam?' });
    await user.click(within(dialog).getByRole('button', { name: 'Unlink' }));
    expect(await within(dialog).findByRole('alert')).toHaveTextContent('Set a password first.');
    expect(screen.getByRole('dialog', { name: 'Unlink Keycloak from sam?' })).toBeInTheDocument();
  });

  it('shows no card for a person without one', async () => {
    mockFetch(signedInRoutes({ 'GET /admin/users/2': { body: samDetail() } }));
    renderApp('/people/user/2?tab=sign-in');
    await screen.findByRole('heading', { name: 'Password' });
    expect(screen.queryByRole('button', { name: 'Unlink' })).toBeNull();
  });
});
