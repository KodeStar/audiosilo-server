import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { setToken } from '@/api/token';
import { mockFetch, type MockRoute } from '@/test/fetch-mock';
import { admin, idle, libraries, sam, setupEnvelope, stats } from '@/test/fixtures';
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
  it.each([0, 1, 4])('counts %i podcast episodes', async (count) => {
    mockFetch(
      routes({
        'GET /admin/libraries': {
          body: { libraries: libraries([{ kind: 'podcasts', book_count: count }]) },
        },
      }),
    );
    renderApp('/library/libraries');
    const card = await screen.findByRole('listitem', { name: 'Fiction' });
    expect(
      within(card).getByText(`${count} ${count === 1 ? 'episode' : 'episodes'}`),
    ).toBeInTheDocument();
  });

  it('uses podcast copy, hides book settings and preserves them when switching back', async () => {
    const calls = mockFetch(
      routes({
        'POST /admin/libraries': {
          status: 201,
          body: { id: 3, name: 'Talks', root: '/mnt/podcasts', kind: 'podcasts' },
        },
      }),
    );
    renderApp('/library/libraries');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Add library' }));
    const dialog = await screen.findByRole('dialog', { name: 'Add a library' });
    await user.type(within(dialog).getByLabelText('Skip these files and folders'), 'Extras/');
    await user.selectOptions(within(dialog).getByLabelText('Book details come from'), 'path');
    await user.selectOptions(within(dialog).getByLabelText('Kind'), 'podcasts');
    expect(within(dialog).getByPlaceholderText('e.g. Podcasts')).toBeInTheDocument();
    expect(within(dialog).getByPlaceholderText('/mnt/podcasts')).toBeInTheDocument();
    expect(within(dialog).queryByLabelText('Skip these files and folders')).not.toBeInTheDocument();
    expect(within(dialog).queryByLabelText('Book details come from')).not.toBeInTheDocument();
    expect(within(dialog).getByText(/Feed checks run separately/)).toBeInTheDocument();
    await user.selectOptions(within(dialog).getByLabelText('Kind'), 'books');
    expect(within(dialog).getByLabelText('Skip these files and folders')).toHaveValue('Extras/');
    expect(within(dialog).getByLabelText('Book details come from')).toHaveValue('path');
    await user.selectOptions(within(dialog).getByLabelText('Kind'), 'podcasts');
    await user.type(within(dialog).getByLabelText('Name'), 'Talks');
    await user.type(within(dialog).getByLabelText('Folder'), '/mnt/podcasts');
    await user.click(within(dialog).getByRole('button', { name: 'Add library and scan' }));
    await screen.findByText('Added Talks');
    expect(calls.find((c) => c.method === 'POST' && c.path === '/admin/libraries')?.body).toEqual({
      kind: 'podcasts',
      name: 'Talks',
      root: '/mnt/podcasts',
      scan_schedule: '',
    });
  });

  it('preserves stored book settings when editing a podcast library', async () => {
    const calls = mockFetch(
      routes({
        'GET /admin/libraries': {
          body: {
            libraries: libraries([
              { kind: 'podcasts', metadata_source: 'path', ignore_patterns: ['Extras/'] },
            ]),
          },
        },
        'PATCH /admin/libraries/1': {
          body: { id: 1, name: 'Talks', root: '/mnt/tank/fiction', kind: 'podcasts' },
        },
      }),
    );
    renderApp('/library/libraries');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Actions for Fiction' }));
    await user.click(await screen.findByRole('menuitem', { name: 'Edit library...' }));
    const dialog = await screen.findByRole('dialog', { name: 'Edit Fiction' });
    expect(within(dialog).getByLabelText('Kind')).toBeDisabled();
    expect(within(dialog).queryByLabelText('Book details come from')).not.toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Save changes' }));
    await waitFor(() =>
      expect(calls.some((c) => c.method === 'PATCH' && c.path === '/admin/libraries/1')).toBe(true),
    );
    const body = calls.find((c) => c.method === 'PATCH' && c.path === '/admin/libraries/1')?.body;
    expect(body).not.toHaveProperty('metadata_source');
    expect(body).not.toHaveProperty('ignore_patterns');
  });

  it('places a localized root overlap error at the folder field', async () => {
    mockFetch(
      routes({
        'POST /admin/libraries': {
          status: 409,
          body: { code: 'root_overlaps', error: 'raw root overlap' },
        },
      }),
    );
    renderApp('/library/libraries');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Add library' }));
    const dialog = await screen.findByRole('dialog', { name: 'Add a library' });
    await user.type(within(dialog).getByLabelText('Name'), 'Pods');
    await user.type(within(dialog).getByLabelText('Folder'), '/mnt/podcasts');
    await user.click(within(dialog).getByRole('button', { name: 'Add library and scan' }));
    expect(
      await within(dialog).findByText(
        'This folder overlaps a podcast library. Choose separate folders.',
      ),
    ).toBeInTheDocument();
    expect(within(dialog).getByLabelText('Folder')).toHaveAttribute('aria-invalid', 'true');
  });

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
    expect(post?.body).toEqual({
      kind: 'books',
      name: 'drama',
      root: '/mnt/drama',
      scan_schedule: '',
      ignore_patterns: [],
      metadata_source: 'tags',
    });
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
                {
                  scan: running ? { ...idle, running: true, total: 4, done: 1, indexed: 1 } : idle,
                },
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
    // The plain export names no paths and carries no tags.
    expect(exp?.query.get('include')).toBeNull();
    expect(createObjectURL).toHaveBeenCalled();
  });

  it('exports with paths, tags and descriptions only after a warning', async () => {
    vi.stubGlobal(
      'URL',
      Object.assign(URL, { createObjectURL: vi.fn(() => 'blob:export'), revokeObjectURL: vi.fn() }),
    );
    const calls = mockFetch(
      routes({
        'GET /admin/libraries/1/export': {
          raw: '{"format":"audiosilo-books"}',
          headers: {
            'Content-Type': 'application/json',
            'Content-Disposition':
              'attachment; filename="audiosilo-fiction-2026-10-03-private.json"',
          },
        },
      }),
    );
    renderApp('/library/libraries');
    const user = userEvent.setup();
    const fiction = await screen.findByRole('listitem', { name: 'Fiction' });
    await user.click(within(fiction).getByRole('button', { name: 'Actions for Fiction' }));
    await user.click(
      await screen.findByRole('menuitem', { name: 'Export with paths, tags and descriptions...' }),
    );
    const dialog = await screen.findByRole('dialog', {
      name: 'Export Fiction with paths, tags and descriptions',
    });
    expect(
      within(dialog).getByText(/Don't attach this file to a public issue/),
    ).toBeInTheDocument();
    expect(within(dialog).getByText(/Each copy of a book gets its own row/)).toBeInTheDocument();
    expect(calls.some((c) => c.path === '/admin/libraries/1/export')).toBe(false);
    await user.click(
      within(dialog).getByRole('button', { name: 'Export with paths, tags and descriptions' }),
    );
    expect(
      await screen.findByText('audiosilo-fiction-2026-10-03-private.json'),
    ).toBeInTheDocument();
    const exp = calls.find((c) => c.path === '/admin/libraries/1/export');
    // One row per copy, each with its own path, tags and description.
    expect(exp?.query.get('include')).toBe('paths,tags,description');
    expect(exp?.query.get('copies')).toBe('separate');
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
            libraries: libraries([{}, { available: false, scan: { ...idle, running: true } }]),
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

it('creates a podcast library and explains the Bridge requirement', async () => {
  const calls = mockFetch(
    routes({
      'POST /admin/libraries': {
        status: 201,
        body: {
          id: 3,
          kind: 'podcasts',
          name: 'Shows',
          root: '/podcasts',
          default_view: 'hybrid',
          sort_order: 2,
        },
      },
    }),
  );
  renderApp('/library/libraries');
  const user = userEvent.setup();
  await user.click(await screen.findByRole('button', { name: 'Add library' }));
  const dialog = await screen.findByRole('dialog', { name: 'Add a library' });
  expect(
    within(dialog).getByText('Podcasts need a Bridge and the Podcasts module.'),
  ).toBeInTheDocument();
  await user.selectOptions(within(dialog).getByLabelText('Kind'), 'podcasts');
  await user.type(within(dialog).getByLabelText('Name'), 'Shows');
  await user.type(within(dialog).getByLabelText('Folder'), '/podcasts');
  await user.click(within(dialog).getByRole('button', { name: 'Add library and scan' }));
  await waitFor(() =>
    expect(
      calls.find((c) => c.method === 'POST' && c.path === '/admin/libraries')?.body,
    ).toMatchObject({ kind: 'podcasts' }),
  );
});

it('shows the podcast badge and keeps its kind read only on edit', async () => {
  mockFetch(
    routes({ 'GET /admin/libraries': { body: { libraries: libraries([{ kind: 'podcasts' }]) } } }),
  );
  renderApp('/library/libraries');
  const user = userEvent.setup();
  const card = await screen.findByRole('listitem', { name: 'Fiction' });
  expect(within(card).getByText('Podcasts')).toBeInTheDocument();
  await user.click(within(card).getByRole('button', { name: 'Actions for Fiction' }));
  await user.click(await screen.findByRole('menuitem', { name: 'Edit library...' }));
  const field = await screen.findByLabelText('Kind');
  expect(field).toHaveValue('podcasts');
  expect(field).toBeDisabled();
});

describe('libraries, 2.4.0 fixes', () => {
  const created = {
    status: 201,
    body: { id: 3, name: 'Talks', root: '/mnt/podcasts', kind: 'podcasts' },
  };
  const demo = { ...sam, id: 4, username: 'demo', is_demo: true };
  const member = { ...sam, id: 5, username: 'kid', managed_by: { id: 2, username: 'sam' } };

  async function addPodcasts(everyone: boolean) {
    renderApp('/library/libraries');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Add library' }));
    const dialog = await screen.findByRole('dialog', { name: 'Add a library' });
    await user.selectOptions(within(dialog).getByLabelText('Kind'), 'podcasts');
    const box = within(dialog).getByRole('checkbox', { name: 'Give everyone access' });
    expect(box).toBeChecked();
    if (!everyone) await user.click(box);
    await user.type(within(dialog).getByLabelText('Name'), 'Talks');
    await user.type(within(dialog).getByLabelText('Folder'), '/mnt/podcasts');
    await user.click(within(dialog).getByRole('button', { name: 'Add library and scan' }));
    await screen.findByText('Added Talks');
  }
  const grants = (calls: ReturnType<typeof mockFetch>) =>
    calls.filter((c) => c.method === 'POST' && c.path === '/admin/library-access');

  it('gives a new podcast library to every listener, not admins, members or the demo (BRIDGE-07)', async () => {
    const calls = mockFetch(
      routes({
        'POST /admin/libraries': created,
        'GET /admin/users': { body: { users: [admin, sam, demo, member] } },
        'POST /admin/library-access': { status: 204 },
      }),
    );
    await addPodcasts(true);
    await waitFor(() => expect(grants(calls)).toHaveLength(1));
    expect(grants(calls)[0].body).toEqual({ user_id: sam.id, library_id: 3 });
  });

  it('sends the grants one at a time, so only the first makes the library share', async () => {
    const lee = { ...sam, id: 6, username: 'lee' };
    let open = 0;
    let most = 0;
    const calls = mockFetch(
      routes({
        'POST /admin/libraries': created,
        'GET /admin/users': { body: { users: [admin, sam, lee] } },
        'POST /admin/library-access': async () => {
          most = Math.max(most, ++open);
          await new Promise((r) => setTimeout(r, 5));
          open--;
          return { status: 204 };
        },
      }),
    );
    await addPodcasts(true);
    await waitFor(() => expect(grants(calls)).toHaveLength(2));
    expect(grants(calls).map((c) => (c.body as { user_id: number }).user_id)).toEqual([
      sam.id,
      lee.id,
    ]);
    expect(most).toBe(1);
  });

  it('gives it to no one when the box is cleared', async () => {
    const calls = mockFetch(
      routes({
        'POST /admin/libraries': created,
        'GET /admin/users': { body: { users: [admin, sam] } },
      }),
    );
    await addPodcasts(false);
    expect(grants(calls)).toHaveLength(0);
  });

  it('says how many grants failed, the library still added', async () => {
    mockFetch(
      routes({
        'POST /admin/libraries': created,
        'GET /admin/users': { body: { users: [admin, sam] } },
        'POST /admin/library-access': { status: 500, body: { error: 'boom' } },
      }),
    );
    await addPodcasts(true);
    expect(await screen.findByText("Couldn't give 1 person access")).toBeInTheDocument();
  });

  it('keeps the name and folder of a library config.yaml declares (UPG-01)', async () => {
    const calls = mockFetch(
      routes({
        'GET /admin/setup': { body: setupEnvelope({}, { config_libraries: ['Fiction'] }) },
        'PATCH /admin/libraries/1': { body: { id: 1, name: 'Fiction', root: '/media/fiction' } },
      }),
    );
    renderApp('/library/libraries');
    const fiction = await screen.findByRole('listitem', { name: 'Fiction' });
    expect(await within(fiction).findByText('From your config')).toBeInTheDocument();
    const user = userEvent.setup();
    await user.click(within(fiction).getByRole('button', { name: 'Actions for Fiction' }));
    await user.click(await screen.findByRole('menuitem', { name: 'Edit library...' }));
    const dialog = await screen.findByRole('dialog', { name: 'Edit Fiction' });
    expect(within(dialog).getByLabelText('Name')).toHaveAttribute('readonly');
    expect(within(dialog).getByLabelText('Folder')).toHaveAttribute('readonly');
    expect(within(dialog).getByRole('button', { name: 'Browse' })).toBeDisabled();
    expect(within(dialog).getByText(/set in config.yaml/)).toBeInTheDocument();
    // A save sends its other settings only: the name and folder stay the config's.
    await user.click(within(dialog).getByRole('button', { name: 'Save changes' }));
    await waitFor(() => expect(calls.some((c) => c.method === 'PATCH')).toBe(true));
    const sent = calls.find((c) => c.method === 'PATCH')!.body as Record<string, unknown>;
    expect(sent).not.toHaveProperty('name');
    expect(sent).not.toHaveProperty('root');
    expect(sent).toHaveProperty('scan_schedule');
  });

  it('keeps a library config.yaml declares from being deleted, with the reason (server#176)', async () => {
    const calls = mockFetch(
      routes({
        'GET /admin/setup': { body: setupEnvelope({}, { config_libraries: ['Fiction'] }) },
        'DELETE /admin/libraries/2': { status: 204 },
      }),
    );
    renderApp('/library/libraries');
    const user = userEvent.setup();
    const fiction = await screen.findByRole('listitem', { name: 'Fiction' });
    expect(await within(fiction).findByText('From your config')).toBeInTheDocument();
    await user.click(within(fiction).getByRole('button', { name: 'Actions for Fiction' }));
    const locked = await screen.findByRole('menuitem', { name: /Delete library\.\.\./ });
    expect(locked).toHaveAttribute('aria-disabled', 'true');
    expect(within(locked).getByText(/remove it there and restart/)).toBeInTheDocument();
    await user.click(locked);
    expect(screen.queryByRole('dialog', { name: 'Delete Fiction?' })).toBeNull();
    await user.keyboard('{Escape}');
    // A library the console added still deletes.
    const kids = screen.getByRole('listitem', { name: 'Kids' });
    await user.click(within(kids).getByRole('button', { name: 'Actions for Kids' }));
    const item = await screen.findByRole('menuitem', { name: 'Delete library...' });
    expect(item).not.toHaveAttribute('aria-disabled', 'true');
    await user.click(item);
    const dialog = await screen.findByRole('dialog', { name: 'Delete Kids?' });
    await user.type(within(dialog).getByRole('textbox'), 'Kids');
    await user.click(within(dialog).getByRole('button', { name: 'Delete library' }));
    expect(await screen.findByText('Deleted Kids')).toBeInTheDocument();
    expect(calls.some((c) => c.method === 'DELETE' && c.path === '/admin/libraries/1')).toBe(false);
  });

  it("says why the server refused a config library's delete the console didn't know of, and locks it", async () => {
    // The console read the setup before the config declared Fiction.
    let declared: string[] = [];
    const calls = mockFetch(
      routes({
        'GET /admin/setup': () => ({ body: setupEnvelope({}, { config_libraries: declared }) }),
        'DELETE /admin/libraries/1': () => {
          declared = ['Fiction'];
          return {
            status: 409,
            body: {
              error: "This library is declared in the server's config.",
              code: 'config_library',
            },
          };
        },
      }),
    );
    renderApp('/library/libraries');
    const user = userEvent.setup();
    const fiction = await screen.findByRole('listitem', { name: 'Fiction' });
    await user.click(within(fiction).getByRole('button', { name: 'Actions for Fiction' }));
    await user.click(await screen.findByRole('menuitem', { name: 'Delete library...' }));
    const dialog = await screen.findByRole('dialog', { name: 'Delete Fiction?' });
    await user.type(within(dialog).getByRole('textbox'), 'Fiction');
    await user.click(within(dialog).getByRole('button', { name: 'Delete library' }));
    // The delete's own reason, not the rename's "Change them there".
    expect(
      await within(dialog).findByText(/^Declared in config\.yaml .*remove it there and restart/),
    ).toBeInTheDocument();
    // The setup is read again: the card now says where the library comes from.
    expect(await within(fiction).findByText('From your config')).toBeInTheDocument();
    expect(calls.filter((c) => c.path === '/admin/setup').length).toBeGreaterThan(1);
  });

  it("lets a library the console added be renamed, and puts the server's config refusal on the name", async () => {
    mockFetch(
      routes({
        'PATCH /admin/libraries/1': {
          status: 409,
          body: { error: 'declared in config', code: 'config_library' },
        },
      }),
    );
    renderApp('/library/libraries');
    const fiction = await screen.findByRole('listitem', { name: 'Fiction' });
    expect(within(fiction).queryByText('From your config')).toBeNull();
    const user = userEvent.setup();
    await user.click(within(fiction).getByRole('button', { name: 'Actions for Fiction' }));
    await user.click(await screen.findByRole('menuitem', { name: 'Edit library...' }));
    const dialog = await screen.findByRole('dialog', { name: 'Edit Fiction' });
    const name = within(dialog).getByLabelText('Name');
    expect(name).not.toHaveAttribute('readonly');
    await user.clear(name);
    await user.type(name, 'Novels');
    await user.click(within(dialog).getByRole('button', { name: 'Save changes' }));
    await waitFor(() => expect(name).toHaveAttribute('aria-invalid', 'true'));
    expect(within(dialog).getByText(/set in config.yaml/)).toBeInTheDocument();
  });
});
