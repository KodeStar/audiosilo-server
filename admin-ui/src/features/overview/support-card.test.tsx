import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { setToken } from '@/api/token';
import { mockFetch, type MockRoute } from '@/test/fetch-mock';
import { systemStatus, updateStatus } from '@/test/fixtures';
import { renderApp } from '@/test/render-app';
import { signedInRoutes } from '@/test/routes';

// The support card on the Overview, the account menu's Support AudioSilo link and
// the sponsor line on the update notice. The server decides when the card shows;
// an answer is sent as-is and hides the card.

const SPONSORS = 'https://github.com/sponsors/KodeStar';

beforeEach(() => setToken('stored'));
afterEach(() => vi.unstubAllGlobals());

function dueRoutes(over: Record<string, MockRoute> = {}) {
  return signedInRoutes({
    'GET /admin/support': { body: { show: true } },
    'POST /admin/support': { body: { show: false } },
    ...over,
  });
}

describe('support card', () => {
  it('stays away while the server says it is not due', async () => {
    mockFetch(signedInRoutes());
    renderApp();
    await screen.findByRole('region', { name: 'Server' });
    expect(screen.queryByRole('region', { name: 'Support AudioSilo' })).not.toBeInTheDocument();
  });

  it('links to GitHub Sponsors in a new tab without hiding itself', async () => {
    const calls = mockFetch(dueRoutes());
    renderApp();
    const card = await screen.findByRole('region', { name: 'Support AudioSilo' });
    expect(
      within(card).getByText(/AudioSilo is free, with nothing held back\./),
    ).toBeInTheDocument();
    const link = within(card).getByRole('link', { name: /Sponsor on GitHub/ });
    expect(link).toHaveAttribute('href', SPONSORS);
    expect(link).toHaveAttribute('target', '_blank');
    expect(link).toHaveAttribute('rel', 'noreferrer noopener');
    link.addEventListener('click', (e) => e.preventDefault()); // jsdom can't open tabs
    await userEvent.setup().click(link);
    expect(screen.getByRole('region', { name: 'Support AudioSilo' })).toBeInTheDocument();
    expect(calls.some((c) => c.method === 'POST' && c.path === '/admin/support')).toBe(false);
  });

  it.each([
    [
      'Not now',
      'snoozed',
      { show: false, until: '2027-04-09T10:00:00Z' },
      'Hidden until Apr 9, 2027',
    ],
    ["I've donated", 'donated', { show: false }, 'Thank you'],
  ])('"%s" sends %s and hides the card', async (button, action, answer, toast) => {
    const calls = mockFetch(dueRoutes({ 'POST /admin/support': { body: answer } }));
    renderApp();
    const card = await screen.findByRole('region', { name: 'Support AudioSilo' });
    await userEvent.setup().click(within(card).getByRole('button', { name: button }));
    await waitFor(() =>
      expect(screen.queryByRole('region', { name: 'Support AudioSilo' })).not.toBeInTheDocument(),
    );
    expect(await screen.findByText(toast)).toBeInTheDocument();
    const post = calls.find((c) => c.method === 'POST' && c.path === '/admin/support');
    expect(post?.body).toEqual({ action });
  });

  it('thanks rather than dates a "Not now" that a donation already outranks', async () => {
    mockFetch(dueRoutes());
    renderApp();
    const card = await screen.findByRole('region', { name: 'Support AudioSilo' });
    await userEvent.setup().click(within(card).getByRole('button', { name: 'Not now' }));
    expect(await screen.findByText('Thank you')).toBeInTheDocument();
  });

  it('stays, with a message, when the answer is not saved', async () => {
    mockFetch(dueRoutes({ 'POST /admin/support': { status: 500, body: { error: 'boom' } } }));
    renderApp();
    const card = await screen.findByRole('region', { name: 'Support AudioSilo' });
    await userEvent.setup().click(within(card).getByRole('button', { name: 'Not now' }));
    expect(await screen.findByText("Couldn't save your answer")).toBeInTheDocument();
    expect(screen.getByRole('region', { name: 'Support AudioSilo' })).toBeInTheDocument();
  });
});

it('keeps Support AudioSilo in the account menu', async () => {
  mockFetch(signedInRoutes());
  renderApp();
  const user = userEvent.setup();
  await user.click(await screen.findByRole('button', { name: 'Account menu' }));
  const item = await screen.findByRole('menuitem', { name: 'Support AudioSilo' });
  expect(item).toHaveAttribute('href', SPONSORS);
  expect(item).toHaveAttribute('target', '_blank');
  expect(item).toHaveAttribute('rel', 'noreferrer noopener');
});

describe('about', () => {
  it('adds the sponsor line to an available update, and a link beside the docs', async () => {
    const latest = {
      version: 'v1.16.0',
      name: 'v1.16.0',
      url: 'https://github.com/KodeStar/audiosilo-server/releases/tag/v1.16.0',
      published_at: '2026-10-03T10:00:00Z',
    };
    mockFetch(
      signedInRoutes({
        'GET /admin/system': {
          body: systemStatus({ update: updateStatus({ update_available: true, latest }) }),
        },
      }),
    );
    renderApp('/server/about');
    expect(await screen.findByText('AudioSilo v1.16.0 is available')).toBeInTheDocument();
    expect(screen.getByText(/AudioSilo is free; sponsors keep it going\./)).toBeInTheDocument();
    // Both open GitHub in a new tab without telling it which server sent them.
    for (const name of ['Sponsor on GitHub', 'Support AudioSilo']) {
      const link = screen.getByRole('link', { name });
      expect(link).toHaveAttribute('href', SPONSORS);
      expect(link).toHaveAttribute('target', '_blank');
      expect(link).toHaveAttribute('rel', 'noreferrer noopener');
    }
  });

  it('has no sponsor line while up to date', async () => {
    mockFetch(signedInRoutes({ 'GET /admin/system': { body: systemStatus() } }));
    renderApp('/server/about');
    expect(await screen.findByText("You're up to date")).toBeInTheDocument();
    expect(screen.queryByText(/sponsors keep it going/)).not.toBeInTheDocument();
  });
});
