import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { setToken } from '@/api/token';
import { mockFetch, type MockRoute } from '@/test/fetch-mock';
import { idle, libraries, stats } from '@/test/fixtures';
import { renderApp } from '@/test/render-app';
import { signedInRoutes } from '@/test/routes';

function routes(over: Record<string, MockRoute> = {}) {
  return signedInRoutes({
    'GET /libraries/1/books': { body: { books: [] } },
    'GET /libraries/2/books': { body: { books: [] } },
    ...over,
  });
}

beforeEach(() => setToken('stored'));
afterEach(() => vi.unstubAllGlobals());

describe('libraries', () => {
  it('lists libraries with their folder, book count and status', async () => {
    mockFetch(routes());
    renderApp('/library/libraries');
    const fiction = await screen.findByRole('listitem', { name: 'Fiction' });
    expect(within(fiction).getByText('/mnt/tank/fiction')).toBeInTheDocument();
    expect(within(fiction).getByText('2,400 books')).toBeInTheDocument();
    expect(within(fiction).getByText('Online')).toBeInTheDocument();
  });

  it('celebrates the safety stop for a library whose folder is unreachable', async () => {
    mockFetch(
      routes({
        'GET /admin/libraries': { body: { libraries: libraries([{}, { available: false }]) } },
      }),
    );
    renderApp('/library/libraries');
    const kids = await screen.findByRole('listitem', { name: 'Kids' });
    expect(within(kids).getByText('Folder unavailable')).toBeInTheDocument();
    expect(within(kids).getByText('Safety stop: nothing was deleted')).toBeInTheDocument();
    expect(within(kids).getByText(/can't read \/mnt\/nas\/kids/)).toBeInTheDocument();
    expect(within(kids).getByRole('button', { name: 'Retry' })).toBeInTheDocument();
    // The shell says so too.
    expect(screen.getByText('Kids is offline')).toBeInTheDocument();
  });

  it("doesn't claim books were kept when an unreadable library has none", async () => {
    mockFetch(
      routes({
        'GET /admin/libraries': {
          body: { libraries: libraries([{}, { available: false, book_count: 0 }]) },
        },
      }),
    );
    renderApp('/library/libraries');
    const kids = await screen.findByRole('listitem', { name: 'Kids' });
    expect(within(kids).getByText("AudioSilo can't read this folder")).toBeInTheDocument();
    expect(within(kids).queryByText(/Safety stop/)).not.toBeInTheDocument();
  });

  it('adds a library, picking its folder from the server', async () => {
    const calls = mockFetch(
      routes({
        'GET /admin/fs/dirs': (req) =>
          req.query.get('path') === '/mnt'
            ? {
                body: {
                  path: '/mnt',
                  parent: '/',
                  dirs: [{ name: 'drama', path: '/mnt/drama' }],
                },
              }
            : {
                body: { path: '/', dirs: [{ name: 'mnt', path: '/mnt' }] },
              },
        'POST /admin/libraries': {
          status: 201,
          body: { id: 3, name: 'drama', root: '/mnt/drama', default_view: '', sort_order: 2 },
        },
      }),
    );
    renderApp('/library/libraries');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Add library' }));
    const dialog = await screen.findByRole('dialog', { name: 'Add a library' });
    await user.click(within(dialog).getByRole('button', { name: 'Browse' }));
    await user.click(await within(dialog).findByRole('button', { name: 'mnt' }));
    await user.click(await within(dialog).findByRole('button', { name: 'Choose drama' }));
    expect(within(dialog).getByLabelText('Folder')).toHaveValue('/mnt/drama');
    // The name fills in from the folder when it's empty.
    expect(within(dialog).getByLabelText('Name')).toHaveValue('drama');
    await user.click(within(dialog).getByRole('button', { name: 'Add library and scan' }));
    expect(await screen.findByText('Added drama')).toBeInTheDocument();
    const post = calls.find((c) => c.method === 'POST' && c.path === '/admin/libraries');
    expect(post?.body).toEqual({ name: 'drama', root: '/mnt/drama' });
    await waitFor(() =>
      expect(screen.queryByRole('dialog', { name: 'Add a library' })).not.toBeInTheDocument(),
    );
  });

  it("says how to fix a folder the server can't use", async () => {
    mockFetch(
      routes({
        'POST /admin/libraries': {
          status: 409,
          body: { error: 'name already taken', code: 'name_taken' },
        },
      }),
    );
    renderApp('/library/libraries');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Add library' }));
    const dialog = await screen.findByRole('dialog', { name: 'Add a library' });
    await user.click(within(dialog).getByRole('button', { name: 'Add library and scan' }));
    expect(await within(dialog).findByText('Give the library a name.')).toBeInTheDocument();
    await user.type(within(dialog).getByLabelText('Name'), 'Fiction');
    await user.type(within(dialog).getByLabelText('Folder'), '/mnt/x');
    await user.click(within(dialog).getByRole('button', { name: 'Add library and scan' }));
    expect(await within(dialog).findByText(/already in use/)).toBeInTheDocument();
  });

  it('opens the add dialog from the first-run call to action', async () => {
    mockFetch(
      routes({
        'GET /admin/stats': { body: stats({ total_libraries: 0, libraries: [], listening: [] }) },
        'GET /admin/libraries': { body: { libraries: [] } },
      }),
    );
    const { router } = renderApp();
    await userEvent
      .setup()
      .click(await screen.findByRole('link', { name: 'Add your first library' }));
    expect(await screen.findByRole('dialog', { name: 'Add a library' })).toBeInTheDocument();
    expect(router.state.location.pathname).toBe('/library/libraries');
  });

  it('reorders with the move buttons', async () => {
    let order = libraries();
    const calls = mockFetch(
      routes({
        'GET /admin/libraries': () => ({ body: { libraries: order } }),
        'PUT /admin/libraries/order': () => {
          order = libraries().reverse();
          return { body: { libraries: order } };
        },
      }),
    );
    renderApp('/library/libraries');
    await userEvent.setup().click(await screen.findByRole('button', { name: 'Move Fiction down' }));
    await waitFor(() =>
      expect(calls.find((c) => c.path === '/admin/libraries/order')?.body).toEqual({ ids: [2, 1] }),
    );
    const list = screen.getByRole('list', { name: 'Libraries, in the order players show them' });
    await waitFor(() =>
      expect(within(list).getAllByRole('listitem')[0]).toHaveAccessibleName('Kids'),
    );
  });

  it('rescans and reports when the scan finishes', async () => {
    // The scan shows up in the library list (polled every second while one
    // runs): first running, then finished.
    let polls = 0;
    const calls = mockFetch(
      routes({
        'POST /admin/libraries/1/scan': { status: 202, body: { status: 'scan started' } },
        'GET /admin/libraries': () => {
          const started = calls.some((c) => c.method === 'POST');
          const running = started && polls++ < 1;
          return {
            body: {
              libraries: libraries([
                { scan: running ? { running: true, total: 4, done: 1, indexed: 1 } : idle },
              ]),
            },
          };
        },
      }),
    );
    renderApp('/library/libraries');
    const fiction = await screen.findByRole('listitem', { name: 'Fiction' });
    await userEvent.setup().click(within(fiction).getByRole('button', { name: 'Rescan' }));
    expect(await within(fiction).findByText('1 of 4 books checked')).toBeInTheDocument();
    expect(
      await screen.findByText('Finished scanning Fiction', {}, { timeout: 3000 }),
    ).toBeInTheDocument();
  });

  it('deletes a library only after its name is typed', async () => {
    const calls = mockFetch(routes({ 'DELETE /admin/libraries/2': { status: 204 } }));
    renderApp('/library/libraries');
    const user = userEvent.setup();
    const kids = await screen.findByRole('listitem', { name: 'Kids' });
    await user.click(within(kids).getByRole('button', { name: 'Actions for Kids' }));
    await user.click(await screen.findByRole('menuitem', { name: 'Delete library...' }));
    const dialog = await screen.findByRole('dialog', { name: 'Delete Kids?' });
    expect(within(dialog).getByText(/everyone's progress/)).toBeInTheDocument();
    const confirm = within(dialog).getByRole('button', { name: 'Delete library' });
    expect(confirm).toBeDisabled();
    await user.type(within(dialog).getByRole('textbox'), 'Kids');
    await user.click(confirm);
    expect(await screen.findByText('Deleted Kids')).toBeInTheDocument();
    expect(calls.some((c) => c.method === 'DELETE' && c.path === '/admin/libraries/2')).toBe(true);
  });

  it('overrides how a folder is detected', async () => {
    const calls = mockFetch(
      routes({
        'GET /libraries/1/fs': {
          body: {
            path: '',
            total: 2,
            offset: 0,
            entries: [
              {
                name: 'Short Stories',
                path: 'Short Stories',
                is_dir: true,
                is_audio: false,
                size: 0,
                mod_time: 0,
              },
              {
                name: 'loose.mp3',
                path: 'loose.mp3',
                is_dir: false,
                is_audio: true,
                size: 1,
                mod_time: 0,
              },
            ],
          },
        },
        'PUT /admin/libraries/1/folder-override': { body: { status: 'override set' } },
      }),
    );
    renderApp('/library/libraries');
    const user = userEvent.setup();
    const fiction = await screen.findByRole('listitem', { name: 'Fiction' });
    await user.click(within(fiction).getByRole('button', { name: 'Actions for Fiction' }));
    await user.click(await screen.findByRole('menuitem', { name: 'Folder detection...' }));
    const dialog = await screen.findByRole('dialog', { name: 'Folder detection in Fiction' });
    // Files aren't listed: overrides apply to folders.
    expect(within(dialog).queryByText('loose.mp3')).not.toBeInTheDocument();
    await user.selectOptions(
      await within(dialog).findByRole('combobox', { name: 'How to treat Short Stories' }),
      'collection',
    );
    expect(await screen.findByText('Updated Short Stories')).toBeInTheDocument();
    const put = calls.find((c) => c.method === 'PUT' && c.path.endsWith('/folder-override'));
    expect(put?.query.get('path')).toBe('Short Stories');
    expect(put?.body).toEqual({ mode: 'collection' });
  });

  it('downloads the book list with the session header', async () => {
    const createObjectURL = vi.fn(() => 'blob:export');
    vi.stubGlobal('URL', Object.assign(URL, { createObjectURL, revokeObjectURL: vi.fn() }));
    const calls = mockFetch(
      routes({
        'GET /admin/libraries/1/export': {
          raw: '{"format":"audiosilo-books"}',
          headers: {
            'Content-Type': 'application/json',
            'Content-Disposition': 'attachment; filename="audiosilo-fiction-2026-10-03.json"',
          },
        },
      }),
    );
    renderApp('/library/libraries');
    const user = userEvent.setup();
    const fiction = await screen.findByRole('listitem', { name: 'Fiction' });
    await user.click(within(fiction).getByRole('button', { name: 'Actions for Fiction' }));
    await user.click(await screen.findByRole('menuitem', { name: 'Export book list' }));
    expect(await screen.findByText('audiosilo-fiction-2026-10-03.json')).toBeInTheDocument();
    const exp = calls.find((c) => c.path === '/admin/libraries/1/export');
    expect(exp?.headers.Authorization).toBe('Bearer stored');
    expect(createObjectURL).toHaveBeenCalled();
  });

  it('shows the empty state with no libraries', async () => {
    mockFetch(routes({ 'GET /admin/libraries': { body: { libraries: [] } } }));
    renderApp('/library/libraries');
    expect(await screen.findByRole('heading', { name: 'No libraries yet' })).toBeInTheDocument();
  });
});

describe('libraries, review regressions', () => {
  it('keeps the newest order when two reorders answer out of order', async () => {
    let order = libraries([{}, {}]).concat({
      ...libraries()[0],
      id: 3,
      name: 'Audio drama',
      sort_order: 2,
    });
    const answers: ((ids: number[]) => void)[] = [];
    const calls = mockFetch(
      routes({
        'GET /admin/libraries': () => ({ body: { libraries: order } }),
        'GET /libraries/3/books': { body: { books: [] } },
        'GET /libraries/3/cover': { status: 404 },
        'PUT /admin/libraries/order': (req) =>
          new Promise((resolve) =>
            answers.push(() => {
              const ids = (req.body as { ids: number[] }).ids;
              order = ids.map((id) => order.find((l) => l.id === id)!);
              resolve({ body: { libraries: order } });
            }),
          ),
      }),
    );
    renderApp('/library/libraries');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Move Fiction down' }));
    await user.click(await screen.findByRole('button', { name: 'Move Fiction down' }));
    await waitFor(() => expect(calls.filter((c) => c.method === 'PUT')).toHaveLength(2));
    const list = screen.getByRole('list', { name: 'Libraries, in the order players show them' });
    const names = () =>
      within(list)
        .getAllByRole('listitem')
        .map((li) => li.getAttribute('aria-labelledby'));
    const newest = names();
    // The second move answers first, then the first (older) one.
    answers[1]([]);
    answers[0]([]);
    await waitFor(() => expect(names()).toEqual(newest));
    expect(newest[2]).toBe('library-1-name'); // Fiction ended up last
  });

  it('shows Scanning, not Unavailable, while a retry scans a folder that just came back', async () => {
    mockFetch(
      routes({
        'GET /admin/libraries': {
          body: {
            libraries: libraries([
              {},
              { available: false, scan: { running: true, total: 0, done: 0, indexed: 0 } },
            ]),
          },
        },
      }),
    );
    renderApp('/library/libraries');
    const kids = await screen.findByRole('listitem', { name: 'Kids' });
    expect(within(kids).getAllByText('Scanning').length).toBeGreaterThan(0);
    expect(within(kids).queryByText('Folder unavailable')).not.toBeInTheDocument();
  });

  it('reports a scan that finished before the list was fetched again', async () => {
    mockFetch(
      routes({
        'POST /admin/libraries/1/scan': { status: 202, body: { status: 'scan started' } },
        // A tiny library: the scan is already over when the list comes back.
      }),
    );
    renderApp('/library/libraries');
    const fiction = await screen.findByRole('listitem', { name: 'Fiction' });
    await userEvent.setup().click(within(fiction).getByRole('button', { name: 'Rescan' }));
    expect(await screen.findByText('Finished scanning Fiction')).toBeInTheDocument();
  });

  it("keeps a folder's error on screen instead of jumping back to the root", async () => {
    mockFetch(
      routes({
        'GET /admin/fs/dirs': (req) => {
          const path = req.query.get('path');
          if (path === '/mnt/locked') {
            return {
              status: 404,
              body: { error: 'folder not found or not readable', code: 'folder_unreadable' },
            };
          }
          return { body: { path: '/', dirs: [{ name: 'locked', path: '/mnt/locked' }] } };
        },
      }),
    );
    renderApp('/library/libraries');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Add library' }));
    const dialog = await screen.findByRole('dialog', { name: 'Add a library' });
    await user.click(within(dialog).getByRole('button', { name: 'Browse' }));
    await user.click(await within(dialog).findByRole('button', { name: 'locked' }));
    expect(await within(dialog).findByText(/can't read it/)).toBeInTheDocument();
    expect(
      within(dialog).getByText('locked', { selector: '[aria-current="location"]' }),
    ).toBeInTheDocument();
  });
});
