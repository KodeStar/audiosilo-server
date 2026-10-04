import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { setToken } from '@/api/token';
import { mockFetch, type MockRoute } from '@/test/fetch-mock';
import { fictionGrant, kidsShare, users } from '@/test/fixtures';
import { renderApp } from '@/test/render-app';
import { signedInRoutes } from '@/test/routes';

function routes(over: Record<string, MockRoute> = {}) {
  return signedInRoutes({
    'GET /admin/users': { body: { users } },
    'GET /admin/shares': { body: { shares: [kidsShare, fictionGrant] } },
    ...over,
  });
}

beforeEach(() => setToken('stored'));

describe('shares', () => {
  it('shows the selected share: its folders and its people', async () => {
    mockFetch(routes());
    renderApp('/people/shares');
    const detail = await screen.findByRole('region', { name: 'Cosy mysteries' });
    expect(within(detail).getByText('Fiction › Agatha Christie')).toBeInTheDocument();
    expect(await within(detail).findByText('sam')).toBeInTheDocument();
    // Whole-library grants are listed apart, read-only.
    const nav = screen.getByRole('navigation', { name: 'Shares' });
    expect(within(nav).getByText('Whole libraries')).toBeInTheDocument();
    await userEvent.setup().click(within(nav).getByRole('button', { name: /^Fiction/ }));
    const grant = await screen.findByRole('region', { name: 'Fiction' });
    expect(within(grant).queryByRole('button', { name: /Actions for/ })).not.toBeInTheDocument();
  });

  it('creates a share and selects it', async () => {
    const calls = mockFetch(
      routes({
        'POST /admin/shares': {
          status: 201,
          body: { id: 12, name: 'Kids', description: '', read_only: false },
        },
      }),
    );
    const { router } = renderApp('/people/shares');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'New share' }));
    const dialog = await screen.findByRole('dialog', { name: 'New share' });
    await user.click(within(dialog).getByRole('button', { name: 'Create share' }));
    expect(await within(dialog).findByText('Give the share a name.')).toBeInTheDocument();
    await user.type(within(dialog).getByLabelText('Name'), 'Kids');
    await user.click(within(dialog).getByRole('button', { name: 'Create share' }));
    await waitFor(() => expect(router.state.location.search).toEqual({ share: 12 }));
    expect(calls.find((c) => c.method === 'POST')?.body).toEqual({ name: 'Kids' });
  });

  it('adds a folder picked from the library', async () => {
    const calls = mockFetch(
      routes({
        'GET /libraries/1/fs': (req) =>
          req.query.get('path') === ''
            ? {
                body: {
                  path: '',
                  total: 1,
                  offset: 0,
                  entries: [
                    {
                      name: 'Dorothy Sayers',
                      path: 'Dorothy Sayers',
                      is_dir: true,
                      is_audio: false,
                      size: 0,
                      mod_time: 0,
                    },
                  ],
                },
              }
            : { body: { path: 'Dorothy Sayers', total: 0, offset: 0, entries: [] } },
        'POST /admin/shares/7/paths': { status: 204 },
      }),
    );
    renderApp('/people/shares?share=7');
    const user = userEvent.setup();
    const detail = await screen.findByRole('region', { name: 'Cosy mysteries' });
    await user.click(within(detail).getByRole('button', { name: 'Add a folder' }));
    const dialog = await screen.findByRole('dialog', { name: 'Add to Cosy mysteries' });
    await user.click(await within(dialog).findByRole('button', { name: 'Add Dorothy Sayers' }));
    await waitFor(() =>
      expect(calls.find((c) => c.path === '/admin/shares/7/paths')?.body).toEqual({
        library_id: 1,
        path: 'Dorothy Sayers',
      }),
    );
  });

  it('removes a folder and a person, and adds a person', async () => {
    const calls = mockFetch(
      routes({
        'DELETE /admin/shares/7/paths': { status: 204 },
        'DELETE /admin/share-access': { status: 204 },
      }),
    );
    renderApp('/people/shares');
    const user = userEvent.setup();
    const detail = await screen.findByRole('region', { name: 'Cosy mysteries' });
    await user.click(
      within(detail).getByRole('button', { name: 'Remove Fiction › Agatha Christie' }),
    );
    await user.click(within(detail).getByRole('button', { name: 'Take the share away from sam' }));
    await waitFor(() => {
      expect(calls.find((c) => c.path === '/admin/shares/7/paths')?.body).toEqual({
        library_id: 1,
        path: 'Agatha Christie',
      });
      expect(calls.find((c) => c.path === '/admin/share-access')?.body).toEqual({
        user_id: 2,
        share_id: 7,
      });
    });
  });

  it('deletes a share after saying who loses access', async () => {
    const calls = mockFetch(routes({ 'DELETE /admin/shares/7': { status: 204 } }));
    renderApp('/people/shares');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Actions for Cosy mysteries' }));
    await user.click(await screen.findByRole('menuitem', { name: 'Delete share' }));
    const dialog = await screen.findByRole('dialog', { name: 'Delete Cosy mysteries?' });
    expect(within(dialog).getByText(/1 person loses access/)).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Delete share' }));
    await waitFor(() =>
      expect(calls.some((c) => c.method === 'DELETE' && c.path === '/admin/shares/7')).toBe(true),
    );
  });
});
