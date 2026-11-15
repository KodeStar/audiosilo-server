import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { setToken } from '@/api/token';
import type { AdminUser } from '@/api/types';
import { mockFetch, type MockRoute } from '@/test/fetch-mock';
import { sam, samDetail } from '@/test/fixtures';
import { renderApp } from '@/test/render-app';
import { signedInRoutes } from '@/test/routes';

// People > a person > Sign-in: their email (server#235), what a single
// sign-on with a verified email links to on its own.

function routes(user: AdminUser, over: Record<string, MockRoute> = {}) {
  return signedInRoutes({
    'GET /admin/users/2': { body: samDetail({ user }) },
    'GET /admin/devices': { body: { devices: [] } },
    ...over,
  });
}

beforeEach(() => setToken('stored'));

describe('a person’s email', () => {
  it('saves it, trimmed', async () => {
    const calls = mockFetch(
      routes(sam, { 'PATCH /admin/users/2': { body: { ...sam, email: 'sam@example.com' } } }),
    );
    renderApp('/people/user/2?tab=sign-in');
    const user = userEvent.setup();
    const field = await screen.findByRole('textbox', { name: 'Email' });
    const save = screen.getByRole('button', { name: 'Save' });
    expect(save).toBeDisabled();
    await user.type(field, ' sam@example.com ');
    await user.click(save);
    expect(await screen.findByText('Email saved')).toBeInTheDocument();
    expect(calls.find((c) => c.method === 'PATCH')?.body).toEqual({ email: 'sam@example.com' });
  });

  it('shows the server’s refusal under the field', async () => {
    mockFetch(
      routes(sam, {
        'PATCH /admin/users/2': {
          status: 400,
          body: { error: 'not an email address', code: 'invalid_email' },
        },
      }),
    );
    renderApp('/people/user/2?tab=sign-in');
    const user = userEvent.setup();
    await user.type(await screen.findByRole('textbox', { name: 'Email' }), 'nope');
    await user.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByRole('alert')).toHaveTextContent("That isn't an email address.");
    expect(screen.getByRole('textbox', { name: 'Email' })).toHaveValue('nope');
  });

  it('clears it', async () => {
    const withEmail = { ...sam, email: 'sam@example.com' };
    const calls = mockFetch(
      routes(withEmail, { 'PATCH /admin/users/2': { body: { ...sam, email: '' } } }),
    );
    renderApp('/people/user/2?tab=sign-in');
    const user = userEvent.setup();
    const field = await screen.findByRole('textbox', { name: 'Email' });
    expect(field).toHaveValue('sam@example.com');
    await user.clear(field);
    await user.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByText('Email removed')).toBeInTheDocument();
    expect(calls.find((c) => c.method === 'PATCH')?.body).toEqual({ email: '' });
  });

  it('says an admin is never linked on its own', async () => {
    mockFetch(routes({ ...sam, role: 'admin' }));
    renderApp('/people/user/2?tab=sign-in');
    expect(await screen.findByText(/never linked on its own/)).toBeInTheDocument();
  });

  it('is not offered for a household member', async () => {
    const kid: AdminUser = {
      ...sam,
      managed_by: { id: 1, username: 'chris', display_name: '' },
      member_kind: 'grownup',
      member_access: 'all',
    };
    mockFetch(routes(kid));
    renderApp('/people/user/2?tab=sign-in');
    await screen.findByRole('heading', { name: 'Password' });
    await waitFor(() => expect(screen.queryByRole('textbox', { name: 'Email' })).toBeNull());
  });
});
