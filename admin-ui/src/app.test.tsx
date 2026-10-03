import { act, fireEvent, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { getToken, setToken } from '@/api/token';
import { setUnauthorizedHandler } from '@/api/client';
import { toast } from '@/lib/toast';
import { mockFetch } from '@/test/fetch-mock';
import { admin, member, stats } from '@/test/fixtures';
import { signedInRoutes } from '@/test/routes';
import { renderApp } from '@/test/render-app';

afterEach(() => {
  vi.unstubAllGlobals();
  setUnauthorizedHandler(() => {});
});

describe('sign-in gate', () => {
  it('signs an admin in and lands on the overview', async () => {
    mockFetch({
      ...signedInRoutes(),
      'POST /auth/login': { body: { token: 'new-token', user: admin, server_id: 'srv-1' } },
    });
    renderApp();
    const user = userEvent.setup();
    await user.type(await screen.findByLabelText('Username'), 'chris');
    await user.type(screen.getByLabelText('Password'), 'secret');
    await user.click(screen.getByRole('button', { name: 'Sign in' }));
    expect(await screen.findByRole('heading', { level: 1, name: /chris\.$/ })).toBeInTheDocument();
    expect(getToken()).toBe('new-token');
  });

  it('explains wrong credentials', async () => {
    mockFetch({ 'POST /auth/login': { status: 401, body: { error: 'invalid credentials' } } });
    renderApp();
    const user = userEvent.setup();
    await user.type(await screen.findByLabelText('Username'), 'chris');
    await user.type(screen.getByLabelText('Password'), 'nope');
    await user.click(screen.getByRole('button', { name: 'Sign in' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(
      "That username and password don't match",
    );
    expect(getToken()).toBeNull();
  });

  it('refuses a non-admin account and revokes the session it was given', async () => {
    const calls = mockFetch({
      'POST /auth/login': { body: { token: 'member-token', user: member, server_id: 'srv-1' } },
      'POST /auth/logout': { status: 204 },
    });
    renderApp();
    const user = userEvent.setup();
    await user.type(await screen.findByLabelText('Username'), 'sam');
    await user.type(screen.getByLabelText('Password'), 'pw');
    await user.click(screen.getByRole('button', { name: 'Sign in' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('not an administrator');
    const logout = calls.find((c) => c.path === '/auth/logout');
    expect(logout?.headers.Authorization).toBe('Bearer member-token');
    expect(getToken()).toBeNull();
  });

  it('restores a stored admin session without asking to sign in', async () => {
    setToken('stored');
    mockFetch(signedInRoutes());
    renderApp();
    expect(await screen.findByRole('heading', { level: 1, name: /chris\.$/ })).toBeInTheDocument();
    expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
  });

  it('drops a stored session that turned out to be a non-admin', async () => {
    setToken('stored');
    mockFetch(signedInRoutes({ 'GET /me': { body: member } }));
    renderApp();
    expect(await screen.findByRole('alert')).toHaveTextContent('not an administrator');
    expect(getToken()).toBeNull();
  });

  it('says so when a stored session has expired', async () => {
    setToken('stale');
    mockFetch(signedInRoutes({ 'GET /me': { status: 401, body: { error: 'unauthorized' } } }));
    renderApp();
    expect(await screen.findByRole('alert')).toHaveTextContent('Your session ended');
  });

  it('keeps the session and offers a retry when the server is unreachable', async () => {
    setToken('stored');
    let down = true;
    mockFetch(signedInRoutes({ 'GET /me': () => (down ? 'network-error' : { body: admin }) }));
    renderApp();
    expect(
      await screen.findByRole('heading', { name: "Can't reach the server" }),
    ).toBeInTheDocument();
    expect(getToken()).toBe('stored');
    down = false;
    await userEvent.setup().click(screen.getByRole('button', { name: 'Try again' }));
    expect(await screen.findByRole('heading', { level: 1, name: /chris\.$/ })).toBeInTheDocument();
  });

  it('retrying after the token vanished (signed out elsewhere) shows sign-in, not a stuck spinner', async () => {
    setToken('stored');
    mockFetch(signedInRoutes({ 'GET /me': 'network-error' }));
    renderApp();
    await screen.findByRole('heading', { name: "Can't reach the server" });
    localStorage.removeItem('audiosilo_token'); // another tab signed out
    await userEvent.setup().click(screen.getByRole('button', { name: 'Try again' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Your session ended');
  });

  it('signs out', async () => {
    setToken('stored');
    const calls = mockFetch({ ...signedInRoutes(), 'POST /auth/logout': { status: 204 } });
    renderApp();
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Account menu' }));
    await user.click(await screen.findByRole('menuitem', { name: 'Sign out' }));
    expect(await screen.findByLabelText('Username')).toBeInTheDocument();
    expect(calls.some((c) => c.path === '/auth/logout')).toBe(true);
    expect(getToken()).toBeNull();
  });
});

describe('overview', () => {
  beforeEach(() => setToken('stored'));

  it('shows who is listening, totals, recent listening and the server card', async () => {
    mockFetch(signedInRoutes());
    renderApp();
    const live = await screen.findByRole('region', { name: 'Listening now' });
    expect(await within(live).findByText('Project Hail Mary')).toBeInTheDocument();
    expect(screen.getByText('1 person is listening right now.')).toBeInTheDocument();
    expect(screen.getByText('3,249')).toBeInTheDocument();
    const recent = screen.getByRole('region', { name: 'Recent listening' });
    expect(within(recent).getByText('The Wild Robot')).toBeInTheDocument();
    expect(within(recent).getByText('maya · finished')).toBeInTheDocument();
    expect(screen.getByText('Fiction')).toBeInTheDocument();
    const server = screen.getByRole('region', { name: 'Server' });
    expect(await within(server).findByText('v0.9.2')).toBeInTheDocument();
  });

  it('renders covers from data: URLs fetched with the header, never a token in a URL', async () => {
    const calls = mockFetch(
      signedInRoutes({
        'GET /libraries/1/cover': {
          raw: new Uint8Array([0x89, 0x50, 0x4e, 0x47]),
          headers: { 'Content-Type': 'image/png' },
        },
      }),
    );
    renderApp();
    const img = await screen.findByAltText('Project Hail Mary'); // the <img>, not the loading skeleton
    expect(img.getAttribute('src')).toBe('data:image/png;base64,iVBORw==');
    const cover = calls.find((c) => c.path === '/libraries/1/cover');
    expect(cover?.headers.Authorization).toBe('Bearer stored');
    expect(calls.every((c) => !c.query.has('token'))).toBe(true);
    expect(document.body.innerHTML).not.toContain('token=');
  });

  it('welcomes a server with no libraries', async () => {
    mockFetch(
      signedInRoutes({
        'GET /admin/stats': { body: stats({ total_libraries: 0, libraries: [], listening: [] }) },
      }),
    );
    renderApp();
    expect(
      await screen.findByRole('heading', { name: "Let's put your audiobooks on the shelf." }),
    ).toBeInTheDocument();
  });

  it('shows an error with a retry when stats fail', async () => {
    mockFetch(
      signedInRoutes({
        'GET /admin/stats': { status: 500, body: { error: 'could not count books' } },
      }),
    );
    renderApp();
    expect(await screen.findByText("The overview couldn't load")).toBeInTheDocument();
    expect(screen.getByText('could not count books')).toBeInTheDocument();
  });
});

describe('navigation', () => {
  beforeEach(() => setToken('stored'));

  it('deep-links a destination section to its placeholder', async () => {
    mockFetch(signedInRoutes());
    renderApp('/library/authors');
    expect(
      await screen.findByRole('heading', { name: 'Authors is on its way' }),
    ).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Authors' })).toHaveAttribute('aria-current', 'page');
  });

  it('404s an unknown section', async () => {
    mockFetch(signedInRoutes());
    renderApp('/library/nope');
    expect(
      await screen.findByRole('heading', { name: "There's nothing here" }),
    ).toBeInTheDocument();
  });

  it('marks the active destination', async () => {
    mockFetch(signedInRoutes());
    renderApp('/health');
    const nav = await screen.findAllByRole('navigation', { name: 'Primary' });
    for (const n of nav)
      expect(within(n).getByRole('link', { name: 'Health' })).toHaveAttribute(
        'aria-current',
        'page',
      );
  });
});

describe('command palette', () => {
  beforeEach(() => setToken('stored'));

  it('opens with Ctrl+K, filters, and runs a settings command', async () => {
    mockFetch(signedInRoutes());
    renderApp();
    await screen.findByRole('heading', { level: 1, name: /chris\.$/ });
    fireEvent.keyDown(window, { key: 'k', ctrlKey: true });
    const input = await screen.findByPlaceholderText('Search pages, settings, or type a command');
    expect(screen.getByRole('option', { name: /Rescan Fiction/ })).toBeInTheDocument();
    const user = userEvent.setup();
    await user.type(input, 'dark theme');
    await user.keyboard('{Enter}');
    await waitFor(() => expect(document.documentElement).toHaveAttribute('data-theme', 'dark'));
    expect(localStorage.getItem('audiosilo.admin.theme')).toBe('dark');
  });

  it('rescans a library from an action', async () => {
    const calls = mockFetch({
      ...signedInRoutes(),
      'POST /admin/libraries/1/scan': { status: 202, body: { status: 'scan started' } },
    });
    renderApp();
    await screen.findByRole('heading', { level: 1, name: /chris\.$/ });
    fireEvent.keyDown(window, { key: 'k', metaKey: true });
    await userEvent.setup().click(await screen.findByRole('option', { name: /Rescan Fiction/ }));
    expect(await screen.findByText('Rescanning Fiction')).toBeInTheDocument();
    expect(calls.some((c) => c.method === 'POST' && c.path === '/admin/libraries/1/scan')).toBe(
      true,
    );
  });

  it("navigates to a section that's only offered while searching", async () => {
    mockFetch(signedInRoutes());
    const { router } = renderApp();
    await screen.findByRole('heading', { level: 1, name: /chris\.$/ });
    fireEvent.keyDown(window, { key: '/' });
    const user = userEvent.setup();
    await user.type(await screen.findByRole('combobox'), 'narrators');
    await user.keyboard('{Enter}');
    await waitFor(() => expect(router.state.location.pathname).toBe('/library/narrators'));
  });
});

// The console runs under style-src 'self' with no nonce. Base UI (CSPProvider
// disableStyleElements), cmdk (we never render Command.Dialog) and the toast
// must leave no <style> element behind while the busiest UI is open.
describe('CSP at runtime', () => {
  it('renders no <style> elements with the palette, menus and a toast open', async () => {
    setToken('stored');
    mockFetch(signedInRoutes());
    renderApp();
    await screen.findByRole('heading', { level: 1, name: /chris\.$/ });
    fireEvent.keyDown(window, { key: 'k', ctrlKey: true });
    await screen.findByRole('combobox');
    act(() => {
      toast.add({ title: 'Hello', description: 'World' });
    });
    await screen.findByText('Hello');
    expect(document.querySelectorAll('style')).toHaveLength(0);
    fireEvent.keyDown(document.activeElement ?? window, { key: 'Escape' });
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Account menu' }));
    await screen.findByRole('menu');
    expect(document.querySelectorAll('style')).toHaveLength(0);
  });
});

describe('CSP at runtime, 1b screens', () => {
  it('renders no <style> elements with the sortable list and a dialog open', async () => {
    setToken('stored');
    mockFetch(
      signedInRoutes({
        'GET /libraries/1/books': { body: { books: [] } },
        'GET /libraries/2/books': { body: { books: [] } },
      }),
    );
    renderApp('/library/libraries');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Add library' }));
    await screen.findByRole('dialog', { name: 'Add a library' });
    expect(document.querySelectorAll('style')).toHaveLength(0);
  });
});

describe('theme', () => {
  it('updates the top-bar theme icon when the OS scheme flips under "system"', async () => {
    let dark = false;
    const listeners = new Set<() => void>();
    vi.stubGlobal('matchMedia', (query: string) => ({
      get matches() {
        return query.includes('dark') && dark;
      },
      media: query,
      addEventListener: (_: string, fn: () => void) => listeners.add(fn),
      removeEventListener: (_: string, fn: () => void) => listeners.delete(fn),
    }));
    setToken('stored');
    mockFetch(signedInRoutes());
    renderApp();
    const button = await screen.findByRole('button', { name: 'Theme: Match system' });
    expect(button.querySelector('.lucide-sun')).not.toBeNull();
    dark = true;
    act(() => listeners.forEach((fn) => fn()));
    await waitFor(() => expect(button.querySelector('.lucide-moon')).not.toBeNull());
    expect(document.documentElement).toHaveAttribute('data-theme', 'dark');
  });
});
