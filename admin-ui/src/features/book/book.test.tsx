import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { keys } from '@/api/hooks';
import { setToken } from '@/api/token';
import type { AdminBookDetail, BookEditRequest, MatchCandidate } from '@/api/types';
import { mockFetch, type MockRequest, type MockRoute } from '@/test/fetch-mock';
import { serverInfo } from '@/test/fixtures';
import { bookDetail } from '@/test/library-fixtures';
import { renderApp } from '@/test/render-app';
import { signedInRoutes } from '@/test/routes';

const BOOK_PATH = 'Brandon Sanderson/The Stormlight Archive/01 - The Way of Kings';
const URL = `/library/book?library=1&path=${encodeURIComponent(BOOK_PATH)}`;

/** A detail with some fields changed (the PATCH answer). */
function withFields(base: AdminBookDetail, set: Partial<Record<string, string>>): AdminBookDetail {
  const next = structuredClone(base);
  for (const [f, v] of Object.entries(set)) {
    const field = next.fields[f as keyof typeof next.fields];
    field.value = v ?? '';
    field.source = 'edited';
    field.locked = true;
    if (f === 'title') next.book.title = v ?? '';
  }
  return next;
}

function routes(detail: AdminBookDetail, over: Record<string, MockRoute> = {}) {
  return signedInRoutes({
    'GET /admin/libraries/1/book': { body: detail },
    'PATCH /admin/libraries/1/book': (req) => ({
      body: withFields(detail, (req.body as BookEditRequest).set ?? {}),
    }),
    ...over,
  });
}

const patches = (calls: MockRequest[]) =>
  calls.filter((c) => c.method === 'PATCH' && c.path === '/admin/libraries/1/book');

const row = (name: string) => screen.getByRole('group', { name });

beforeEach(() => setToken('stored'));
afterEach(() => vi.unstubAllGlobals());

describe('book page', () => {
  it('shows the book with where each value came from', async () => {
    mockFetch(routes(bookDetail()));
    renderApp(URL);
    expect(
      await screen.findByRole('heading', { level: 1, name: 'The Way of Kings' }),
    ).toBeInTheDocument();
    expect(screen.getByText('The Stormlight Archive · Book 1')).toBeInTheDocument();
    expect(within(row('Title')).getByText('File tag')).toBeInTheDocument();
    expect(within(row('Series')).getByText('Path')).toBeInTheDocument();
    expect(within(row('ASIN')).getByText('Community')).toBeInTheDocument();
    expect(within(row('Published')).getByText('Not set. Click to add')).toBeInTheDocument();
    // Breadcrumb, chapters, listeners, access and the disk section.
    const crumbs = screen.getByRole('navigation', { name: 'Breadcrumb' });
    expect(within(crumbs).getByText('Fiction')).toBeInTheDocument();
    expect(within(crumbs).getByText('Brandon Sanderson')).toBeInTheDocument();
    expect(screen.getByText('2 chapters across 1 file · 45h 30m')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'sam' })).toHaveAttribute(
      'href',
      '/admin/people/user/2',
    );
    expect(screen.getByText('via Brandon Sanderson')).toBeInTheDocument();
    expect(screen.getByText(`/mnt/tank/fiction/${BOOK_PATH}`)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Preview rename' })).toBeDisabled();
  });

  it('saves edits through the save bar and its diff', async () => {
    const calls = mockFetch(routes(bookDetail()));
    renderApp(URL);
    const user = userEvent.setup();
    await user.click(
      // Named by field and value, so a list of values says which field each edits.
      await within(await screen.findByRole('group', { name: 'Title' })).findByRole('button', {
        name: 'Title The Way of Kings',
      }),
    );
    const input = within(row('Title')).getByRole('textbox', { name: 'Title' });
    await user.clear(input);
    await user.type(input, 'Kings{Enter}');
    expect(within(row('Title')).getByText('Unsaved')).toBeInTheDocument();
    // Emptying an optional field is an edit too.
    await user.click(within(row('Narrator')).getByRole('button'));
    await user.clear(within(row('Narrator')).getByRole('textbox'));
    await user.keyboard('{Enter}');

    const bar = screen.getByRole('toolbar', { name: 'Unsaved changes' });
    expect(within(bar).getByText('2 unsaved changes')).toBeInTheDocument();
    await user.click(within(bar).getByRole('button', { name: 'Review and save' }));
    const dialog = await screen.findByRole('dialog', { name: 'Save these changes?' });
    expect(within(dialog).getByText('The Way of Kings')).toBeInTheDocument();
    expect(within(dialog).getByText('Kings')).toBeInTheDocument();
    expect(patches(calls)).toHaveLength(0);
    await user.click(within(dialog).getByRole('button', { name: 'Save 2 changes' }));

    expect(await screen.findByText('Saved 2 changes to Kings')).toBeInTheDocument();
    expect(patches(calls).map((c) => c.body)).toEqual([{ set: { title: 'Kings', narrator: '' } }]);
    expect(screen.queryByRole('toolbar', { name: 'Unsaved changes' })).not.toBeInTheDocument();
    expect(within(row('Title')).getByText('Edited')).toBeInTheDocument();
  });

  it('says how to fix a value before saving, and maps a refusal onto its field', async () => {
    const calls = mockFetch(
      routes(bookDetail(), {
        'PATCH /admin/libraries/1/book': {
          status: 400,
          body: { error: 'isbn: checksum mismatch', code: 'invalid_override', field: 'isbn' },
        },
      }),
    );
    renderApp(URL);
    const user = userEvent.setup();
    await user.click(
      await within(await screen.findByRole('group', { name: 'Published' })).findByRole('button'),
    );
    await user.type(within(row('Published')).getByRole('textbox'), 'soon{Enter}');
    expect(within(row('Published')).getByRole('alert')).toHaveTextContent(
      'Use a year, like 2010, or 2010-08 or 2010-08-31.',
    );
    expect(screen.getByRole('button', { name: 'Review and save' })).toBeDisabled();

    // Fixed; the server then refuses another field.
    await user.click(within(row('Published')).getByRole('button'));
    const published = within(row('Published')).getByRole('textbox');
    await user.clear(published);
    await user.type(published, '2010{Enter}');
    await user.click(within(row('ISBN')).getByRole('button'));
    await user.type(within(row('ISBN')).getByRole('textbox'), '0765326353{Enter}');
    await user.click(screen.getByRole('button', { name: 'Review and save' }));
    await user.click(
      within(await screen.findByRole('dialog')).getByRole('button', { name: 'Save 2 changes' }),
    );
    expect(await within(row('ISBN')).findByRole('alert')).toHaveTextContent(
      'isbn: checksum mismatch',
    );
    expect(patches(calls)[0].body).toEqual({ set: { published: '2010', isbn: '0765326353' } });
    // The edits are kept, still unsaved.
    expect(screen.getByText('2 unsaved changes')).toBeInTheDocument();
  });

  it('reverts a locked field to the file, with an undo', async () => {
    const detail = bookDetail();
    detail.fields.title = {
      value: 'Kings',
      source: 'edited',
      scanned: 'The Way of Kings',
      locked: true,
    };
    const reverted = structuredClone(detail);
    reverted.fields.title = {
      ...detail.fields.title,
      value: 'The Way of Kings',
      source: 'tag',
      locked: false,
    };
    const calls = mockFetch(
      routes(detail, {
        'PATCH /admin/libraries/1/book': (req) => ({
          body: (req.body as BookEditRequest).revert ? reverted : detail,
        }),
      }),
    );
    renderApp(URL);
    const user = userEvent.setup();
    const revert = await screen.findByRole('button', { name: 'Revert Title to the file' });
    expect(revert).toHaveAttribute('title', 'File tag: The Way of Kings');
    await user.click(revert);
    expect(await screen.findByText('Title reverted to the file')).toBeInTheDocument();
    await waitFor(() => expect(within(row('Title')).getByText('File tag')).toBeInTheDocument());
    await user.click(screen.getByRole('button', { name: 'Undo' }));
    expect(await screen.findByText('Title restored')).toBeInTheDocument();
    expect(patches(calls).map((c) => c.body)).toEqual([
      { revert: ['title'] },
      { set: { title: 'Kings' }, source: 'edited' },
    ]);
  });

  it('renames a chapter in place and saves it at once', async () => {
    const calls = mockFetch(routes(bookDetail()));
    renderApp(URL);
    const user = userEvent.setup();
    await user.click(
      await screen.findByRole('button', {
        name: 'Rename chapter Prelude to the Stormlight Archive',
      }),
    );
    const input = screen.getByRole('textbox', { name: 'Chapter title' });
    await user.clear(input);
    await user.type(input, 'Prelude{Enter}');
    expect(await screen.findByText('Chapter renamed')).toBeInTheDocument();
    // The edited chapter can go back to its file title.
    await user.click(
      screen.getByRole('button', { name: 'Revert chapter Prologue: To Kill to the file' }),
    );
    await waitFor(() => expect(patches(calls)).toHaveLength(2));
    expect(patches(calls).map((c) => c.body)).toEqual([
      { chapters: { set: { 0: 'Prelude' } } },
      { chapters: { revert: [1] } },
    ]);
  });

  it('uploads a custom cover as the file itself', async () => {
    const calls = mockFetch(
      routes(bookDetail(), { 'PUT /admin/libraries/1/cover': { body: { status: 'cover set' } } }),
    );
    const { queryClient } = renderApp(URL);
    // A book list cached from the Library screen (it carries has_cover).
    queryClient.setQueryData(keys.bookList({ sort: 'title' }), { pages: [], pageParams: [] });
    const user = userEvent.setup();
    await screen.findByRole('heading', { level: 1, name: 'The Way of Kings' });
    const image = new File([new Uint8Array([0xff, 0xd8, 0xff])], 'cover.jpg', {
      type: 'image/jpeg',
    });
    await user.upload(screen.getByLabelText('Upload an image...'), image);
    expect(await screen.findByText('Cover saved')).toBeInTheDocument();
    const put = calls.find((c) => c.method === 'PUT');
    expect(put?.path).toBe('/admin/libraries/1/cover');
    expect(put?.query.get('path')).toBe(BOOK_PATH);
    expect(put?.headers['Content-Type']).toBe('image/jpeg');
    const init = vi
      .mocked(fetch)
      .mock.calls.find(([, i]) => i?.method === 'PUT')?.[1] as RequestInit;
    expect(init.body).toBe(image);
    // The lists and counts that say whether the book has a cover refresh too.
    expect(queryClient.getQueryState(keys.bookList({ sort: 'title' }))?.isInvalidated).toBe(true);
  });

  it('refuses an oversized cover before uploading', async () => {
    const calls = mockFetch(routes(bookDetail()));
    renderApp(URL);
    const user = userEvent.setup();
    await screen.findByRole('heading', { level: 1, name: 'The Way of Kings' });
    const big = new File([new Uint8Array(5 * 1024 * 1024 + 1)], 'big.png', { type: 'image/png' });
    await user.upload(screen.getByLabelText('Upload an image...'), big);
    expect(
      await screen.findByText('That image is larger than 5 MB. Pick a smaller one.'),
    ).toBeInTheDocument();
    expect(calls.some((c) => c.method === 'PUT')).toBe(false);
  });

  it('hides matching when community metadata is off, and says where to turn it on', async () => {
    mockFetch(
      routes(bookDetail(), {
        'GET /server': {
          body: { ...serverInfo, capabilities: { ...serverInfo.capabilities, metadata: false } },
        },
      }),
    );
    renderApp(URL);
    expect(
      await screen.findByText('Community metadata is off on this server.'),
    ).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /community/i })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Compare fields' })).not.toBeInTheDocument();
  });

  it("says plainly when the book isn't in the library", async () => {
    mockFetch(
      signedInRoutes({
        'GET /admin/libraries/1/book': {
          status: 404,
          body: { error: 'no book at that path', code: 'book_not_found' },
        },
      }),
    );
    renderApp(URL);
    expect(await screen.findByText("This book isn't in the library")).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Back to the library' })).toHaveAttribute(
      'href',
      '/admin/library',
    );
  });

  it('treats a link that names no book as not found', async () => {
    const calls = mockFetch(signedInRoutes());
    renderApp('/library/book');
    expect(await screen.findByText("This book isn't in the library")).toBeInTheDocument();
    expect(calls.some((c) => c.path.endsWith('/book'))).toBe(false);
  });
});

const candidate: MatchCandidate = {
  work_id: 'the-way-of-kings',
  title: 'The Way of Kings',
  authors: [{ id: 'a1', name: 'Brandon Sanderson' }],
  first_published: '2010-08-31',
  description: 'Roshar is a world of stone and storms.',
  series: [{ name: 'The Stormlight Archive', position: '1' }],
  cover_url: 'https://meta.audiosilo.app/covers/wok.jpg',
  web_url: 'https://meta.audiosilo.app/works/the-way-of-kings',
  recordings: [
    {
      id: 'r1',
      narrators: [
        { id: 'n1', name: 'Michael Kramer' },
        { id: 'n2', name: 'Kate Reading' },
      ],
      runtime_min: 2730,
      publisher: 'Macmillan Audio',
      asins: ['B003P2WO5E'],
      isbns: [],
    },
  ],
  recording_id: 'r1',
  score: 100,
};

describe('match with community metadata', () => {
  function matchRoutes(detail: AdminBookDetail, over: Record<string, MockRoute> = {}) {
    return routes(detail, {
      'GET /admin/libraries/1/book/match': { body: { candidates: [candidate] } },
      ...over,
    });
  }

  it('accepts only the ticked fields as community values, own edits unticked', async () => {
    const detail = bookDetail();
    detail.fields.narrator = { ...detail.fields.narrator, source: 'edited', locked: true };
    const calls = mockFetch(matchRoutes(detail));
    renderApp(URL);
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Compare with community' }));
    const dialog = await screen.findByRole('dialog', { name: 'Match with community metadata' });
    const radio = await within(dialog).findByRole('radio', { name: /The Way of Kings/ });
    expect(radio).toBeChecked();
    expect(within(dialog).getByText('100% match')).toBeInTheDocument();
    expect(within(dialog).getByText('Length matches your files')).toBeInTheDocument();
    // Remote cover art is never loaded (the CSP blocks it): a generated cover stands in.
    expect(dialog.querySelector('img')).toBeNull();
    // With no search typed, the book's own facts are searched.
    const search = calls.find((c) => c.path === '/admin/libraries/1/book/match');
    expect([...search!.query.keys()]).toEqual(['path']);

    await user.click(within(dialog).getByRole('button', { name: 'Compare fields' }));
    const compare = await screen.findByRole('dialog', {
      name: 'Compare with community metadata',
    });
    expect(within(compare).getByRole('checkbox', { name: 'Accept Narrator' })).not.toBeChecked();
    expect(within(compare).getByRole('checkbox', { name: 'Accept Published' })).toBeChecked();
    const description = within(compare).getByRole('checkbox', { name: 'Accept Description' });
    expect(description).toBeChecked();
    expect(within(compare).queryByRole('checkbox', { name: 'Accept Title' })).toBeNull();
    await user.click(description);
    await user.click(within(compare).getByRole('button', { name: 'Accept 1 field' }));

    expect(await screen.findByText('Accepted 1 field from the community')).toBeInTheDocument();
    expect(screen.getByText('Your own edits were left alone.')).toBeInTheDocument();
    expect(patches(calls).map((c) => c.body)).toEqual([
      { set: { published: '2010' }, source: 'community' },
    ]);
  });

  it('searches by an ASIN pasted into the box', async () => {
    const calls = mockFetch(matchRoutes(bookDetail()));
    renderApp(URL);
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Compare with community' }));
    const dialog = await screen.findByRole('dialog');
    const box = within(dialog).getByRole('textbox', { name: 'Search the community database' });
    expect(box).toHaveValue('The Way of Kings Brandon Sanderson');
    await user.clear(box);
    await user.type(box, 'b003p2wo5e{Enter}');
    await waitFor(() =>
      expect(
        calls.some((c) => c.path.endsWith('/book/match') && c.query.get('asin') === 'B003P2WO5E'),
      ).toBe(true),
    );
  });

  it("says so when the community service isn't answering", async () => {
    mockFetch(
      matchRoutes(bookDetail(), {
        'GET /admin/libraries/1/book/match': { status: 502, body: { error: 'bad gateway' } },
      }),
    );
    renderApp(URL);
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Compare with community' }));
    expect(
      await screen.findByText("meta.audiosilo.app isn't answering. Try again in a minute."),
    ).toBeInTheDocument();
  });

  it('waits for unsaved edits before matching', async () => {
    mockFetch(matchRoutes(bookDetail()));
    renderApp(URL);
    const user = userEvent.setup();
    await user.click(
      await within(await screen.findByRole('group', { name: 'Author' })).findByRole('button'),
    );
    await user.type(within(row('Author')).getByRole('textbox'), ' Jr{Enter}');
    expect(screen.getByRole('button', { name: 'Compare with community' })).toBeDisabled();
    expect(screen.getByText('Save or discard your changes before matching.')).toBeInTheDocument();
  });

  it('renders no <style> elements with the match dialog open', async () => {
    mockFetch(matchRoutes(bookDetail()));
    renderApp(URL);
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Compare with community' }));
    await within(await screen.findByRole('dialog')).findByRole('radio');
    expect(document.querySelectorAll('style')).toHaveLength(0);
  });
});
