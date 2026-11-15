import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { setToken } from '@/api/token';
import type { AdminSettings, OIDCStatus } from '@/api/types';
import { mockFetch, type MockRoute } from '@/test/fetch-mock';
import { settingsWith } from '@/test/fixtures';
import { renderApp } from '@/test/render-app';
import { patchEcho, signedInRoutes } from '@/test/routes';
import { signinLook } from './settings-model';

// Settings > Sign-in (server#235): the single sign-on's status and redirect
// URI, the provider read only, who may sign in saved here.

const signin: AdminSettings['signin'] = {
  ...settingsWith().signin,
  enabled: true,
  name: 'Authentik',
  issuer: 'https://auth.example/application/o/audiosilo/',
  client_id: 'audiosilo',
  client_secret_set: true,
};

function status(over: Partial<OIDCStatus> = {}): OIDCStatus {
  return {
    enabled: true,
    configured: true,
    name: 'Authentik',
    issuer: signin.issuer,
    client_id: 'audiosilo',
    client_secret_set: true,
    redirect_uri: 'https://books.example/api/v1/auth/oidc/callback',
    scopes: ['openid', 'profile', 'email', 'groups'],
    username_claim: 'preferred_username',
    groups_claim: 'groups',
    discovery: { state: 'ok', checked_at: '2026-11-15T10:00:00.000Z' },
    ...over,
  };
}

function routes(over: Record<string, MockRoute> = {}) {
  return signedInRoutes({
    'GET /admin/settings': { body: settingsWith({ signin }) },
    'GET /admin/oidc/status': { body: status() },
    ...over,
  });
}

beforeEach(() => setToken('stored'));

describe('Settings > Sign-in', () => {
  it('shows the status, the redirect URI to register and the provider read only', async () => {
    mockFetch(routes());
    renderApp('/server?topic=signin');
    expect(await screen.findByText('Connected to the provider')).toBeInTheDocument();
    expect(screen.getByRole('textbox', { name: 'Redirect URI' })).toHaveValue(
      'https://books.example/api/v1/auth/oidc/callback',
    );
    expect(screen.getByText('https://auth.example/application/o/audiosilo/')).toBeInTheDocument();
    expect(screen.getByText('Authentik')).toBeInTheDocument();
    // The provider is never a form field here, and the secret never shows.
    expect(screen.queryByRole('textbox', { name: 'Issuer' })).toBeNull();
    expect(screen.queryByRole('textbox', { name: 'Client ID' })).toBeNull();
    expect(screen.getByText('Client secret').closest('div')?.parentElement).toHaveTextContent(
      'Set',
    );
  });

  it('says why when it can not run, and what the provider answered', async () => {
    mockFetch(
      routes({
        'GET /admin/oidc/status': {
          body: status({
            configured: false,
            problem: 'auth.oidc needs public_url',
            discovery: undefined,
            redirect_uri: '',
          }),
        },
      }),
    );
    renderApp('/server?topic=signin');
    expect(await screen.findByText('Not set up correctly')).toBeInTheDocument();
    expect(screen.getByText('auth.oidc needs public_url')).toBeInTheDocument();
    expect(screen.getByText(/needs the server's public address/)).toBeInTheDocument();
    expect(screen.queryByRole('textbox', { name: 'Redirect URI' })).toBeNull();
  });

  it('saves who may sign in, only what changed', async () => {
    const base = settingsWith({ signin });
    const calls = mockFetch(routes({ 'PATCH /admin/settings': patchEcho(base) }));
    renderApp('/server?topic=signin');
    const user = userEvent.setup();
    const auto = await screen.findByRole('switch', { name: 'Make accounts for new people' });
    // The group only matters while accounts are made.
    expect(screen.queryByRole('textbox', { name: 'Only for this group' })).toBeNull();
    await user.click(auto);
    await user.type(screen.getByRole('textbox', { name: 'Only for this group' }), 'family');
    const admins = screen.getByRole('textbox', { name: 'Admin group' });
    await user.type(admins, 'admins');
    await user.click(
      within(admins.closest('section') as HTMLElement).getByRole('button', {
        name: 'Save changes',
      }),
    );
    await waitFor(() =>
      expect(calls.find((c) => c.method === 'PATCH')?.body).toEqual({
        signin: { auto_create: true, allowed_group: 'family', admin_group: 'admins' },
      }),
    );
    // A save can change whether single sign-on runs (public_url): read it again.
    const patchAt = calls.findIndex((c) => c.method === 'PATCH');
    await waitFor(() =>
      expect(calls.slice(patchAt).some((c) => c.path === '/admin/oidc/status')).toBe(true),
    );
  });

  it('locks a group the environment sets', async () => {
    mockFetch(
      routes({
        'GET /admin/settings': {
          body: settingsWith({
            signin,
            locked: { 'signin.admin_group': 'AUDIOSILO_OIDC_ADMIN_GROUP' },
          }),
        },
      }),
    );
    renderApp('/server?topic=signin');
    expect(await screen.findByRole('textbox', { name: 'Admin group' })).toBeDisabled();
    expect(screen.getByText('Set by AUDIOSILO_OIDC_ADMIN_GROUP')).toBeInTheDocument();
  });
});

describe('signinLook', () => {
  it('reads each state', () => {
    expect(signinLook(status({ enabled: false })).key).toBe('settings.signin.state.off');
    expect(signinLook(status({ configured: false })).tone).toBe('bad');
    expect(signinLook(status()).tone).toBe('ok');
    expect(signinLook(status({ discovery: { state: 'error', error: 'x' } })).key).toBe(
      'settings.signin.state.error',
    );
    expect(signinLook(status({ discovery: { state: 'pending' } })).key).toBe(
      'settings.signin.state.pending',
    );
  });
});
