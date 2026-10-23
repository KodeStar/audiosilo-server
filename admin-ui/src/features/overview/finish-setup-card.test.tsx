import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { setToken } from '@/api/token';
import type { SetupPatch, SetupState } from '@/api/types';
import { mockFetch } from '@/test/fetch-mock';
import { setupEnvelope } from '@/test/fixtures';
import { renderApp } from '@/test/render-app';
import { signedInRoutes } from '@/test/routes';

// The Overview's "Finish setting up" card: while the first-run flow is
// unfinished, and after it while a step is still skipped, until an admin
// dismisses it.

beforeEach(() => setToken('stored'));
afterEach(() => {
  vi.unstubAllGlobals();
  sessionStorage.clear();
});

const CARD = { name: 'Finish setting up' };

function routes(state: Partial<SetupState>) {
  return signedInRoutes({
    'GET /admin/setup': { body: setupEnvelope(state) },
    'PUT /admin/setup': (req) => ({
      body: { finished: true, dismissed: false, steps: {}, ...state, ...(req.body as SetupPatch) },
    }),
  });
}

describe('finish setting up card', () => {
  it('lists the skipped steps, each a link back into the flow', async () => {
    mockFetch(
      routes({
        finished: true,
        dismissed: false,
        steps: { folder: 'done', abs: 'skipped', bridge: 'skipped' },
      }),
    );
    renderApp();
    const card = await screen.findByRole('region', CARD);
    expect(
      within(card).getByText('You skipped 2 steps of setting up.', { exact: false }),
    ).toBeVisible();
    const links = within(card).getAllByRole('link');
    expect(links.map((l) => l.textContent)).toEqual([
      'Coming from Audiobookshelf?',
      'Add a Bridge',
    ]);
    expect(links[0]).toHaveAttribute('href', '/admin/setup?step=abs');
  });

  it('Dismiss hides it for good', async () => {
    const calls = mockFetch(
      routes({ finished: true, dismissed: false, steps: { reach: 'skipped' } }),
    );
    renderApp();
    const card = await screen.findByRole('region', CARD);
    await userEvent.setup().click(within(card).getByRole('button', { name: 'Dismiss' }));
    await waitFor(() => expect(screen.queryByRole('region', CARD)).not.toBeInTheDocument());
    const put = calls.find((c) => c.method === 'PUT' && c.path === '/admin/setup');
    expect(put?.body).toEqual({ dismissed: true });
  });

  it('while the flow is unfinished ("Finish later"), links back into it', async () => {
    // This session was already sent to the flow, so the Overview stays.
    sessionStorage.setItem('audiosilo.setup.redirected', '1');
    mockFetch(routes({ finished: false, dismissed: false, steps: {} }));
    renderApp();
    const card = await screen.findByRole('region', CARD);
    expect(
      within(card).getByText("You haven't finished setting up this server yet.", { exact: false }),
    ).toBeVisible();
    const link = within(card).getByRole('link', { name: 'Continue setting up' });
    expect(link).toHaveAttribute('href', '/admin/setup');
  });

  it('leaves out a skipped folder step once the server has a library', async () => {
    mockFetch(
      routes({ finished: true, dismissed: false, steps: { folder: 'skipped', abs: 'skipped' } }),
    );
    renderApp();
    const card = await screen.findByRole('region', CARD);
    expect(
      within(card)
        .getAllByRole('link')
        .map((l) => l.textContent),
    ).toEqual(['Coming from Audiobookshelf?']);
  });

  it.each([
    ['dismissed', { finished: true, dismissed: true, steps: { abs: 'skipped' } }],
    [
      'only the folder was skipped and a library exists',
      { finished: true, dismissed: false, steps: { folder: 'skipped' } },
    ],
    ['nothing was skipped', { finished: true, dismissed: false, steps: { abs: 'done' } }],
  ] as const)('stays away when %s', async (_, state) => {
    mockFetch(routes(state));
    renderApp();
    await screen.findByRole('region', { name: 'Server' });
    expect(screen.queryByRole('region', CARD)).not.toBeInTheDocument();
  });
});
