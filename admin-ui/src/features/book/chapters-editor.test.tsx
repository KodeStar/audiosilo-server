import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { setToken } from '@/api/token';
import type { AdminBookDetail, ChapterEditRequest } from '@/api/types';
import { mockFetch, type MockRequest, type MockRoute } from '@/test/fetch-mock';
import { bookDetail } from '@/test/library-fixtures';
import { renderApp } from '@/test/render-app';
import { signedInRoutes } from '@/test/routes';

const BOOK_PATH = 'Brandon Sanderson/The Stormlight Archive/01 - The Way of Kings';
const URL = `/library/book?library=1&path=${encodeURIComponent(BOOK_PATH)}`;
const BASIS = { files: [{ path: '01.m4b', size: 48_000_000, duration_ms: 3_000_000 }] };

function routes(d: AdminBookDetail, over: Record<string, MockRoute> = {}) {
  return signedInRoutes({
    'GET /admin/libraries/1/book': { body: d },
    'POST /cast/token': { status: 201, body: { token: 'media-tok', expires_at: 'x' } },
    ...over,
  });
}

const puts = (calls: MockRequest[]) =>
  calls.filter((c) => c.method === 'PUT' && c.path === '/admin/libraries/1/book/chapters');

async function openEditor() {
  const user = userEvent.setup();
  await user.click(await screen.findByRole('button', { name: 'Edit chapters' }));
  return user;
}

beforeEach(() => setToken('stored'));
afterEach(() => vi.unstubAllGlobals());

describe('the chapter editor', () => {
  it('edits, adds and deletes chapters and saves the list against its basis', async () => {
    const saved = bookDetail({ chapter_source: 'edited' });
    const calls = mockFetch(
      routes(bookDetail(), { 'PUT /admin/libraries/1/book/chapters': { body: saved } }),
    );
    renderApp(URL);
    const user = await openEditor();

    // The file's first chapter keeps its time and can't be deleted.
    expect(screen.queryByRole('textbox', { name: 'Start of chapter 1' })).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Delete chapter 1' })).toBeDisabled();
    const save = screen.getByRole('button', { name: 'Save chapters' });
    expect(save).toBeDisabled();

    const title = screen.getByRole('textbox', { name: 'Title of chapter 2' });
    await user.clear(title);
    await user.type(title, 'Prologue');
    const start = screen.getByRole('textbox', { name: 'Start of chapter 2' });
    await user.clear(start);
    await user.type(start, '20:00.5{Enter}');
    // Halfway between chapter 2 (20:00.5) and the end (50:00).
    await user.click(screen.getByRole('button', { name: 'Add a chapter after chapter 2' }));
    expect(screen.getByRole('textbox', { name: 'Title of chapter 3' })).toHaveValue('New chapter');
    await user.click(screen.getByRole('button', { name: 'Add a chapter after chapter 3' }));
    await user.click(screen.getByRole('button', { name: 'Delete chapter 4' }));
    // The player moves to the chapter before the deleted one.
    expect(screen.getByRole('button', { name: 'Listen from chapter 3' })).toHaveAttribute(
      'aria-pressed',
      'true',
    );

    await user.click(save);
    await waitFor(() => expect(puts(calls)).toHaveLength(1));
    expect(puts(calls)[0].query.get('path')).toBe(BOOK_PATH);
    expect(puts(calls)[0].body as ChapterEditRequest).toEqual({
      basis: BASIS,
      chapters: [
        { start: 0, title: 'Prelude to the Stormlight Archive' },
        { start: 1200.5, title: 'Prologue' },
        { start: 2100.25, title: 'New chapter' },
      ],
    });
    expect(await screen.findByText('Chapters saved')).toBeInTheDocument();
    // Back to the card, the chapters marked as edited here.
    expect(await screen.findByText('Chapters edited on this server.')).toBeInTheDocument();
  });

  it('says why a save was refused, rule by rule, and when the files changed', async () => {
    let answer: { status: number; body: unknown } = {
      status: 400,
      body: { error: 'chapters out of order', code: 'chapters_invalid', detail: 'order' },
    };
    mockFetch(routes(bookDetail(), { 'PUT /admin/libraries/1/book/chapters': () => answer }));
    renderApp(URL);
    const user = await openEditor();
    await user.type(screen.getByRole('textbox', { name: 'Title of chapter 2' }), '!');
    const save = screen.getByRole('button', { name: 'Save chapters' });
    await user.click(save);
    expect(
      await screen.findByText(
        'Chapters start at least a second apart. Move or delete the marked ones.',
      ),
    ).toBeInTheDocument();

    answer = { status: 409, body: { error: 'files changed', code: 'chapters_basis' } };
    await user.click(save);
    expect(
      await screen.findByText(
        /The book’s files changed while you were editing, so nothing was saved/,
      ),
    ).toBeInTheDocument();
  });

  it('checks the list before a save, and refuses a shift out of a file', async () => {
    const calls = mockFetch(routes(bookDetail()));
    renderApp(URL);
    const user = await openEditor();
    const start = screen.getByRole('textbox', { name: 'Start of chapter 2' });
    await user.clear(start);
    await user.type(start, '0:00.5{Enter}');
    expect(
      screen.getByText('Chapters start at least a second apart. Move or delete the marked ones.'),
    ).toBeInTheDocument();
    expect(screen.getByRole('textbox', { name: 'Start of chapter 2' })).toHaveAttribute(
      'aria-invalid',
      'true',
    );
    expect(screen.getByRole('button', { name: 'Save chapters' })).toBeDisabled();

    await user.type(screen.getByRole('textbox', { name: 'Shift all by (seconds)' }), '3000');
    await user.click(screen.getByRole('button', { name: 'Shift' }));
    expect(
      screen.getByText(
        'Nothing was shifted: chapter 2 would leave its file. Shift by less, or move that chapter by hand.',
      ),
    ).toBeInTheDocument();
    expect(puts(calls)).toHaveLength(0);
  });

  it('previews pasted timestamps before they replace the list', async () => {
    const calls = mockFetch(
      routes(bookDetail(), { 'PUT /admin/libraries/1/book/chapters': { body: bookDetail() } }),
    );
    renderApp(URL);
    const user = await openEditor();
    await user.click(screen.getByRole('button', { name: 'Paste timestamps' }));
    const dialog = await screen.findByRole('dialog', { name: 'Paste timestamps' });
    await user.type(
      within(dialog).getByRole('textbox', { name: 'Chapter list' }),
      '0:00 Intro{Enter}Part one 10:00{Enter}nonsense{Enter}2:00:00 Late',
    );
    expect(within(dialog).getByText('2 chapters will replace the list')).toBeInTheDocument();
    expect(
      within(dialog).getByText('Lines without a time or a title, left out: 3.'),
    ).toBeInTheDocument();
    expect(
      within(dialog).getByText('Lines past the end of the book, left out: 4.'),
    ).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Replace chapters' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(screen.getByRole('textbox', { name: 'Title of chapter 1' })).toHaveValue('Intro');
    expect(screen.getByRole('textbox', { name: 'Title of chapter 2' })).toHaveValue('Part one');

    await user.click(screen.getByRole('button', { name: 'Save chapters' }));
    await waitFor(() => expect(puts(calls)).toHaveLength(1));
    expect((puts(calls)[0].body as ChapterEditRequest).chapters).toEqual([
      { start: 0, title: 'Intro' },
      { start: 600, title: 'Part one' },
    ]);
  });

  it('plays the row’s file with a book media token and sets a start at the playhead', async () => {
    const calls = mockFetch(
      routes(bookDetail(), { 'PUT /admin/libraries/1/book/chapters': { body: bookDetail() } }),
    );
    const { container } = renderApp(URL);
    const user = await openEditor();
    await user.click(screen.getByRole('button', { name: 'Listen from chapter 2' }));
    await waitFor(() => expect(container.querySelector('audio')).not.toBeNull());
    const audio = container.querySelector('audio')!;
    const src = new window.URL(audio.getAttribute('src')!, 'http://admin.test');
    expect(src.pathname).toBe('/api/v1/libraries/1/stream');
    expect(src.searchParams.get('path')).toBe(`${BOOK_PATH}/01.m4b`);
    // Never the session token in a URL.
    expect(src.searchParams.get('token')).toBe('media-tok');
    const mint = calls.find((c) => c.path === '/cast/token');
    // The editor's kind of token: it never counts against the admin's casts.
    expect(mint?.body).toEqual({ library_id: 1, path: BOOK_PATH, editor: true });

    // The file has loaded (the playhead is the row's start until it has), and plays on.
    fireEvent.loadedMetadata(audio);
    Object.defineProperty(audio, 'currentTime', { value: 1500.25, writable: true });
    await user.click(screen.getByRole('button', { name: 'Start chapter 2 at the playhead' }));
    expect(screen.getByRole('textbox', { name: 'Start of chapter 2' })).toHaveValue('25:00.250');
    // A file's first chapter has a fixed time: nothing to set.
    await user.click(screen.getByRole('button', { name: 'Listen from chapter 1' }));
    expect(screen.getByRole('button', { name: 'Start chapter 1 at the playhead' })).toBeDisabled();

    await user.click(screen.getByRole('button', { name: 'Save chapters' }));
    await waitFor(() => expect(puts(calls)).toHaveLength(1));
    expect((puts(calls)[0].body as ChapterEditRequest).chapters[1].start).toBe(1500.25);
  });

  it('asks before discarding unsaved changes', async () => {
    mockFetch(routes(bookDetail()));
    renderApp(URL);
    const user = await openEditor();
    await user.type(screen.getByRole('textbox', { name: 'Title of chapter 2' }), '!');
    await user.click(screen.getByRole('button', { name: 'Cancel' }));
    const dialog = await screen.findByRole('dialog', { name: 'Discard your chapter changes?' });
    await user.click(within(dialog).getByRole('button', { name: 'Discard changes' }));
    expect(await screen.findByRole('button', { name: 'Edit chapters' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Save chapters' })).not.toBeInTheDocument();
  });
});

describe('an edited chapter list on the card', () => {
  const edit = { state: 'applied' as const, edited_by: 'kode', edited_at: '2026-11-14T10:00:00Z' };

  it('says who edited the chapters, renames only in the editor, and reverts after a confirm', async () => {
    const d = bookDetail({
      chapter_source: 'edited',
      chapter_edit: edit,
      chapters: bookDetail().chapters.map((c) => ({ ...c, edited: false })),
    });
    const calls = mockFetch(
      routes(d, { 'DELETE /admin/libraries/1/book/chapters': { body: bookDetail() } }),
    );
    renderApp(URL);
    expect(await screen.findByText('Chapters edited on this server.')).toBeInTheDocument();
    expect(screen.getByText(/^Edited by kode, /)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /^Rename chapter/ })).not.toBeInTheDocument();

    const user = userEvent.setup();
    await user.click(screen.getByRole('button', { name: 'Revert edit' }));
    const dialog = await screen.findByRole('dialog', { name: 'Revert the edited chapters?' });
    await user.click(within(dialog).getByRole('button', { name: 'Revert chapters' }));
    await waitFor(() =>
      expect(
        calls.filter((c) => c.method === 'DELETE' && c.path === '/admin/libraries/1/book/chapters'),
      ).toHaveLength(1),
    );
    expect(await screen.findByText('Edited chapters reverted')).toBeInTheDocument();
    expect(screen.queryByText('Chapters edited on this server.')).not.toBeInTheDocument();
  });

  it('says when an edit is set aside because the files changed', async () => {
    mockFetch(routes(bookDetail({ chapter_edit: { ...edit, state: 'dormant', edited_by: null } })));
    renderApp(URL);
    expect(await screen.findByText('Your edited chapters are set aside')).toBeInTheDocument();
    expect(screen.getByText(/The book’s files changed since they were saved/)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Revert edit' })).toBeInTheDocument();
    // The files' chapters are in use: renaming in place still works.
    expect(
      screen.getByRole('button', { name: 'Rename chapter Prelude to the Stormlight Archive' }),
    ).toBeInTheDocument();
  });
});
