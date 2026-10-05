import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { setToken } from '@/api/token';
import type { FolderMode, FsEntry } from '@/api/types';
import { mockFetch, type MockRequest, type MockRoute } from '@/test/fetch-mock';
import { libraries } from '@/test/fixtures';
import { renderApp } from '@/test/render-app';
import { signedInRoutes } from '@/test/routes';

const dir = (path: string, over: Partial<FsEntry> = {}): FsEntry => ({
  name: path.split('/').pop()!,
  path,
  is_dir: true,
  is_audio: false,
  size: 0,
  mod_time: 0,
  ...over,
});
const audio = (path: string): FsEntry => ({
  name: path.split('/').pop()!,
  path,
  is_dir: false,
  is_audio: true,
  size: 400_000_000,
  mod_time: 0,
});

// Fiction: A/ holds the book B/ (three files); Other/ is empty.
function fiction(override?: FolderMode) {
  const fs: Record<string, FsEntry[]> = {
    '': [dir('A'), dir('Other')],
    A: [dir('A/B', { is_book: true, title: 'The Book', author: 'Ann Author', override })],
    'A/B': ['01.mp3', '02.mp3', '03.mp3'].map((f) => audio(`A/B/${f}`)),
    Other: [],
  };
  return (req: MockRequest) => {
    const path = req.query.get('path') ?? '';
    const entries = fs[path];
    return entries
      ? { body: { path, entries, total: entries.length, offset: 0 } }
      : { status: 404, body: { error: 'not found' } };
  };
}

function routes(over: Record<string, MockRoute> = {}) {
  return signedInRoutes({ 'GET /libraries/1/fs': fiction(), ...over });
}

beforeEach(() => setToken('stored'));
afterEach(() => vi.unstubAllGlobals());

describe('folders', () => {
  it('loads the tree lazily and opens a folder', async () => {
    const calls = mockFetch(routes());
    const { router } = renderApp('/library/folders');
    const tree = await screen.findByRole('tree', { name: 'Folders in Fiction' });
    expect(screen.getByText('Pick a folder to see how AudioSilo reads it')).toBeInTheDocument();
    const a = within(tree).getByRole('treeitem', { name: 'A' });
    expect(a).toHaveAttribute('aria-expanded', 'false');
    // Audio files and the children of closed folders aren't fetched or shown.
    expect(
      calls.filter((c) => c.path === '/libraries/1/fs').map((c) => c.query.get('path')),
    ).toEqual(['']);

    await userEvent.setup().click(a);
    expect(await within(tree).findByRole('treeitem', { name: /^B/ })).toHaveAttribute(
      'aria-level',
      '2',
    );
    expect(a).toHaveAttribute('aria-expanded', 'true');
    expect(a).toHaveAttribute('aria-selected', 'true');
    expect(router.state.location.search).toMatchObject({ folder: 'A' });
    // A holds no audio of its own, only the book B in a folder that isn't a disc:
    // nothing to detect, and nothing to join (an author's or a series' folder).
    expect(
      await screen.findByText(/Always one book only joins a folder of disc folders/),
    ).toBeInTheDocument();
    for (const name of [/Automatic/, /Always one book/, /Separate books/]) {
      expect(screen.getByRole('radio', { name })).toHaveAttribute('aria-disabled', 'true');
    }
  });

  it('joins a folder whose audio is only in its disc folders', async () => {
    let override: FolderMode | undefined;
    const calls = mockFetch(
      routes({
        'GET /libraries/1/fs': (req) => {
          const path = req.query.get('path') ?? '';
          const fs: Record<string, FsEntry[]> = {
            // As the server has it once each change's rescan has run.
            '': [dir('Dragonese', { override, is_book: !!override, split_discs: !override })],
            Dragonese: [
              dir('Dragonese/CD1', { is_book: !override }),
              dir('Dragonese/CD2', { is_book: !override }),
            ],
          };
          const entries = fs[path] ?? [];
          return { body: { path, entries, total: entries.length, offset: 0 } };
        },
        'PUT /admin/libraries/1/folder-override': (req) => {
          override = (req.body as { mode: FolderMode }).mode;
          return { body: { status: 'override set' } };
        },
        'DELETE /admin/libraries/1/folder-override': () => {
          override = undefined;
          return { body: { status: 'override cleared' } };
        },
      }),
    );
    renderApp('/library/folders?library=1&folder=Dragonese');
    const user = userEvent.setup();
    expect(
      await screen.findByText(/Here: 2 disc folders become one book, in disc order\./),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/only disc folders \(CD1, CD2\.\.\.\)\. Always one book joins them/),
    ).toBeInTheDocument();
    expect(screen.getByText(/Here: each disc folder is its own book\./)).toBeInTheDocument();
    await user.click(screen.getByRole('radio', { name: /Always one book/ }));
    expect(await screen.findByText('Pinned as one book')).toBeInTheDocument();
    expect(
      screen.getByText(
        'Rescanning Fiction. Listening progress on its disc folders carries over to the one book.',
      ),
    ).toBeInTheDocument();
    const put = calls.find((c) => c.method === 'PUT');
    expect(put?.query.get('path')).toBe('Dragonese');
    expect(put?.body).toEqual({ mode: 'book' });
    await waitFor(() =>
      expect(screen.getByRole('radio', { name: /Always one book/ })).toBeChecked(),
    );

    // Back to automatic: the discs are books again; the joined book keeps its progress.
    await user.click(screen.getByRole('radio', { name: /Automatic/ }));
    expect(
      await screen.findByText(
        'Rescanning Fiction. Each disc folder is its own book again; progress on the one book is kept for it, not split back.',
      ),
    ).toBeInTheDocument();
    expect(calls.find((c) => c.method === 'DELETE')?.query.get('path')).toBe('Dragonese');
  });

  it('offers nothing for a folder with no audio and no folders', async () => {
    mockFetch(routes());
    renderApp('/library/folders?library=1&folder=Other');
    expect(
      await screen.findByText(/holds no audio files of its own, so there is nothing to detect/),
    ).toBeInTheDocument();
    for (const name of [/Automatic/, /Always one book/, /Separate books/]) {
      expect(screen.getByRole('radio', { name })).toHaveAttribute('aria-disabled', 'true');
    }
  });

  it('opens the ancestors of a deep-linked folder and shows how it is read', async () => {
    mockFetch(routes());
    renderApp('/library/folders?library=1&folder=A/B');
    const b = await screen.findByRole('treeitem', { name: /^B/ });
    expect(b).toHaveAttribute('aria-selected', 'true');
    expect(within(b).getByText('Book')).toBeInTheDocument();
    expect(screen.getByRole('treeitem', { name: 'A' })).toHaveAttribute('aria-expanded', 'true');

    expect(await screen.findByRole('heading', { name: 'B' })).toBeInTheDocument();
    expect(screen.getByText('/mnt/tank/fiction/A/B')).toBeInTheDocument();
    expect(screen.getByRole('radio', { name: /Automatic/ })).toBeChecked();
    expect(screen.getByText(/Here: one book with 3 files\./)).toBeInTheDocument();
    expect(screen.getByText(/Here: 3 books\./)).toBeInTheDocument();
    expect(screen.getByText('3 files · 1.2 GB')).toBeInTheDocument();
    expect(screen.getByText('02.mp3')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Open book' })).toHaveAttribute(
      'href',
      '/admin/library/book?library=1&path=A%2FB',
    );
  });

  it('splits a folder into separate books, then puts it back to automatic', async () => {
    let override: FolderMode | undefined;
    const calls = mockFetch(
      routes({
        'GET /libraries/1/fs': (req) => fiction(override)(req),
        'PUT /admin/libraries/1/folder-override': (req) => {
          override = (req.body as { mode: FolderMode }).mode;
          return { body: { status: 'override set' } };
        },
        'DELETE /admin/libraries/1/folder-override': () => {
          override = undefined;
          return { body: { status: 'override cleared' } };
        },
      }),
    );
    renderApp('/library/folders?library=1&folder=A/B');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('radio', { name: /Separate books/ }));
    expect(await screen.findByText('Split into 3 books')).toBeInTheDocument();
    expect(
      screen.getByText('Rescanning Fiction. Listening progress moves with each file.'),
    ).toBeInTheDocument();
    const put = calls.find((c) => c.method === 'PUT');
    expect(put?.query.get('path')).toBe('A/B');
    expect(put?.body).toEqual({ mode: 'collection' });
    // The tree shows the override once the listing is refetched.
    const b = screen.getByRole('treeitem', { name: /^B/ });
    await waitFor(() => expect(within(b).getByText('Separate books')).toBeInTheDocument());
    expect(screen.getByRole('radio', { name: /Separate books/ })).toBeChecked();

    await user.click(screen.getByRole('radio', { name: /Automatic/ }));
    expect(await screen.findByText('Back to automatic')).toBeInTheDocument();
    const del = calls.find((c) => c.method === 'DELETE');
    expect(del?.query.get('path')).toBe('A/B');
    await waitFor(() => expect(screen.getByRole('radio', { name: /Automatic/ })).toBeChecked());
  });

  it('says why a change failed and keeps the folder as it was', async () => {
    mockFetch(
      routes({
        'PUT /admin/libraries/1/folder-override': {
          status: 500,
          body: { error: 'could not save override' },
        },
      }),
    );
    renderApp('/library/folders?library=1&folder=A/B');
    await userEvent.setup().click(await screen.findByRole('radio', { name: /Always one book/ }));
    expect(await screen.findByText("Couldn't change how AudioSilo reads B")).toBeInTheDocument();
    expect(screen.getByText('could not save override')).toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole('radio', { name: /Automatic/ })).toBeChecked());
  });

  it('moves through the tree with the keyboard', async () => {
    mockFetch(routes());
    const { router } = renderApp('/library/folders');
    const user = userEvent.setup();
    const a = await screen.findByRole('treeitem', { name: 'A' });
    expect(a).toHaveAttribute('tabindex', '0');
    act(() => a.focus());
    await user.keyboard('{ArrowRight}');
    const b = await screen.findByRole('treeitem', { name: /^B/ });
    await user.keyboard('{ArrowDown}');
    expect(b).toHaveFocus();
    await user.keyboard('{Enter}');
    await waitFor(() => expect(router.state.location.search).toMatchObject({ folder: 'A/B' }));
    await user.keyboard('{ArrowLeft}');
    expect(a).toHaveFocus();
    await user.keyboard('{ArrowLeft}');
    await waitFor(() => expect(a).toHaveAttribute('aria-expanded', 'false'));
  });

  it('switches library from the picker', async () => {
    const calls = mockFetch(
      routes({
        'GET /libraries/2/fs': { body: { path: '', entries: [dir('Picture books')], total: 1 } },
      }),
    );
    const { router } = renderApp('/library/folders?library=1&folder=A');
    const user = userEvent.setup();
    await screen.findByRole('tree', { name: 'Folders in Fiction' });
    await user.click(screen.getByRole('button', { name: 'Kids' }));
    expect(await screen.findByRole('treeitem', { name: 'Picture books' })).toBeInTheDocument();
    expect(router.state.location.search).toEqual({ library: 2 });
    expect(calls.some((c) => c.path === '/libraries/2/fs')).toBe(true);
  });

  it('celebrates the safety stop for a library whose folder is unreachable', async () => {
    mockFetch(
      routes({
        'GET /admin/libraries': { body: { libraries: libraries([{ available: false }]) } },
      }),
    );
    renderApp('/library/folders');
    expect(
      await screen.findByText("/mnt/tank/fiction isn't reachable. Nothing was deleted."),
    ).toBeInTheDocument();
    expect(screen.queryByRole('tree')).not.toBeInTheDocument();
  });

  it('retries a library whose folders failed to load', async () => {
    let fail = true;
    mockFetch(
      routes({
        'GET /libraries/1/fs': (req) =>
          fail ? { status: 500, body: { error: 'disk on fire' } } : fiction()(req),
      }),
    );
    renderApp('/library/folders');
    expect(await screen.findByText("Couldn't load the folders in Fiction")).toBeInTheDocument();
    expect(screen.getByText('disk on fire')).toBeInTheDocument();
    fail = false;
    await userEvent.setup().click(screen.getByRole('button', { name: 'Try again' }));
    expect(await screen.findByRole('treeitem', { name: 'A' })).toBeInTheDocument();
  });

  it('says so when a library has no folders', async () => {
    mockFetch(
      routes({
        'GET /libraries/1/fs': {
          body: { path: '', entries: [audio('loose.mp3')], total: 1, offset: 0 },
        },
      }),
    );
    renderApp('/library/folders');
    expect(
      await screen.findByRole('heading', { name: 'This library has no folders yet' }),
    ).toBeInTheDocument();
  });

  it("says when a deep-linked folder isn't there any more", async () => {
    mockFetch(routes());
    renderApp('/library/folders?library=1&folder=A/Gone');
    expect(await screen.findByText("Gone isn't here any more")).toBeInTheDocument();
  });
});
