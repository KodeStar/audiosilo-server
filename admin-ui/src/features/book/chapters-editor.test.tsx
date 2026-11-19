import { act, fireEvent, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { setToken } from '@/api/token';
import type { AdminBookDetail, ChapterEditRequest } from '@/api/types';
import { mockFetch, type MockReply, type MockRequest, type MockRoute } from '@/test/fetch-mock';
import { libraries, serverInfo } from '@/test/fixtures';
import { bookDetail } from '@/test/library-fixtures';
import { renderApp } from '@/test/render-app';
import { formatStamp } from './chapters-edit-model';
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

// The editor's <audio> once its stream is asked for: the element is there
// before the media token's mint answers, and its src only then, which a loaded
// run can leave for later than a test's next look at it.
async function streamed(container: HTMLElement) {
  await waitFor(() => expect(container.querySelector('audio')?.getAttribute('src')).toBeTruthy());
  return container.querySelector('audio')!;
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
    // The moved row shows its time in the same field, which keeps the focus.
    expect(start).toHaveValue('20:00.500');
    expect(start).toHaveFocus();
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

  it('offers no Edit chapters on an episode in a podcast library', async () => {
    const podcasts = libraries().map((l) => (l.id === 1 ? { ...l, kind: 'podcasts' as const } : l));
    mockFetch(routes(bookDetail(), { 'GET /admin/libraries': { body: { libraries: podcasts } } }));
    renderApp(URL);
    expect(await screen.findByRole('heading', { name: 'Chapters' })).toBeInTheDocument();
    await waitFor(() =>
      expect(screen.queryByRole('button', { name: 'Edit chapters' })).not.toBeInTheDocument(),
    );
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
    await streamed(container);
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

  it('mints a new token once when the audio fails, then says it can’t play', async () => {
    // The token held may have run out (four hours) or been dropped by the cap.
    let n = 0;
    const calls = mockFetch(
      routes(bookDetail(), {
        'POST /cast/token': () => ({
          status: 201,
          body: { token: `media-tok-${++n}`, expires_at: 'x' },
        }),
      }),
    );
    const { container } = renderApp(URL);
    const user = await openEditor();
    await user.click(screen.getByRole('button', { name: 'Listen from chapter 2' }));
    await streamed(container);
    const mints = () => calls.filter((c) => c.path === '/cast/token');
    expect(mints()).toHaveLength(1);

    fireEvent.error(container.querySelector('audio')!);
    await waitFor(() =>
      expect(container.querySelector('audio')?.getAttribute('src')).toContain('media-tok-2'),
    );
    expect(mints()).toHaveLength(2);
    expect(screen.queryByText(/The audio didn’t load/)).not.toBeInTheDocument();

    // The new token fails too: the file can't play, and no third mint.
    fireEvent.error(container.querySelector('audio')!);
    expect(await screen.findByText(/The audio didn’t load/)).toBeInTheDocument();
    expect(mints()).toHaveLength(2);
  });

  it('plays a file no browser plays as stored through the transcode, asking again on a seek', async () => {
    // Opus in .mka (server#410): the stored file would play as silence.
    const d = bookDetail();
    d.files = d.files.map((f) => ({ ...f, format: 'mka', codec: 'opus', direct_playable: false }));
    mockFetch(routes(d));
    const { container } = renderApp(URL);
    const user = await openEditor();
    await user.click(screen.getByRole('button', { name: 'Listen from chapter 2' }));
    await streamed(container);
    const srcOf = () =>
      new window.URL(container.querySelector('audio')!.getAttribute('src')!, 'http://admin.test')
        .searchParams;
    const row = Number(srcOf().get('t'));
    expect(srcOf().get('transcode')).toBe('1');
    expect(srcOf().get('token')).toBe('media-tok');
    expect(row).toBeGreaterThan(0);

    // The transcode's clock counts from where it started.
    const audio = container.querySelector('audio')!;
    fireEvent.loadedMetadata(audio);
    Object.defineProperty(audio, 'currentTime', { value: 2.5, writable: true });
    fireEvent.timeUpdate(audio);
    await user.click(screen.getByRole('button', { name: 'Forward 1 second' }));
    // A seek asks for the stream again from there, on the same <audio>: its new
    // source lets go of the old stream (its ffmpeg holds a transcode slot).
    await waitFor(() => expect(Number(srcOf().get('t'))).toBe(row + 3.5));
    expect(container.querySelector('audio')).toBe(audio);
    // The new stream's clock starts again.
    audio.currentTime = 0;
    const moved = container.querySelector('audio');
    await user.click(screen.getByRole('button', { name: 'Start chapter 2 at the playhead' }));
    expect(screen.getByRole('textbox', { name: 'Start of chapter 2' })).toHaveValue(
      formatStamp(row + 3.5),
    );
    // The new start is where the transcode plays: it plays on, not asked for again.
    expect(container.querySelector('audio')).toBe(moved);
    expect(Number(srcOf().get('t'))).toBe(row + 3.5);

    // The file's end is never asked for: ffmpeg would have no audio to send.
    fireEvent.change(screen.getByRole('slider'), { target: { value: '3000' } });
    await waitFor(() => expect(Number(srcOf().get('t'))).toBe(2999));
  });

  it('keeps a seek made while an earlier ask is on its way', async () => {
    const d = bookDetail();
    d.files = d.files.map((f) => ({ ...f, format: 'mka', codec: 'opus', direct_playable: false }));
    mockFetch(routes(d));
    const { container } = renderApp(URL);
    const user = await openEditor();
    await user.click(screen.getByRole('button', { name: 'Listen from chapter 2' }));
    await streamed(container);
    const from = () =>
      Number(
        new window.URL(
          container.querySelector('audio')!.getAttribute('src')!,
          'http://admin.test',
        ).searchParams.get('t'),
      );
    const slider = screen.getByRole('slider');
    expect(from()).toBe(1200);
    vi.useFakeTimers();
    try {
      fireEvent.click(screen.getByRole('button', { name: 'Forward 1 second' }));
      // The move is asked for, and another is made before that ask lands.
      act(() => {
        vi.advanceTimersByTime(250);
        fireEvent.change(slider, { target: { value: '1500' } });
      });
      expect(from()).toBe(1201);
      expect(slider).toHaveValue('1500');
      act(() => vi.advanceTimersByTime(250));
    } finally {
      vi.useRealTimers();
    }
    expect(from()).toBe(1500);
  });

  it('puts a transcode back at a row’s start when the row is chosen again', async () => {
    const play = vi.spyOn(HTMLMediaElement.prototype, 'play');
    const d = bookDetail();
    d.files = d.files.map((f) => ({ ...f, format: 'mka', codec: 'opus', direct_playable: false }));
    mockFetch(routes(d));
    const { container } = renderApp(URL);
    const user = await openEditor();
    const from = () =>
      Number(
        new window.URL(
          container.querySelector('audio')!.getAttribute('src')!,
          'http://admin.test',
        ).searchParams.get('t'),
      );
    await user.click(screen.getByRole('button', { name: 'Listen from chapter 2' }));
    await streamed(container);
    fireEvent.loadedMetadata(container.querySelector('audio')!);
    await user.click(screen.getByRole('button', { name: 'Forward 1 second' }));
    await waitFor(() => expect(from()).toBe(1201));
    // Another row, then back: the move made on chapter 2 is forgotten.
    await user.click(screen.getByRole('button', { name: 'Listen from chapter 1' }));
    await waitFor(() => expect(from()).toBe(0));
    await user.click(screen.getByRole('button', { name: 'Listen from chapter 2' }));
    await waitFor(() => expect(from()).toBe(1200));
    expect(play).not.toHaveBeenCalled();
    play.mockRestore();
  });

  it('keeps the audio’s source under StrictMode, which runs a ref’s cleanup on mount', async () => {
    mockFetch(routes(bookDetail()));
    const { container } = renderApp(URL, { strict: true });
    const user = await openEditor();
    await user.click(screen.getByRole('button', { name: 'Listen from chapter 2' }));
    await streamed(container);
    // Past the microtask a real removal lets go of its stream in.
    await Promise.resolve();
    expect(container.querySelector('audio')!.getAttribute('src')).toContain('token=media-tok');
  });

  it('asks for a transcode only to play: a row chosen or a move made while paused starts none', async () => {
    const play = vi.spyOn(HTMLMediaElement.prototype, 'play');
    const d = bookDetail();
    d.files = d.files.map((f) => ({ ...f, format: 'mka', codec: 'opus', direct_playable: false }));
    mockFetch(routes(d));
    const { container } = renderApp(URL);
    const user = await openEditor();
    const audio = () => container.querySelector('audio')!;
    const from = () =>
      Number(
        new window.URL(audio().getAttribute('src')!, 'http://admin.test').searchParams.get('t'),
      );
    const playhead = () => Number((screen.getByRole('slider') as HTMLInputElement).value);
    await user.click(screen.getByRole('button', { name: 'Listen from chapter 2' }));
    await streamed(container);
    // Nothing loads (no ffmpeg, no transcode slot), and the playhead is the row's start all the same.
    expect(audio()).toHaveAttribute('preload', 'none');
    await waitFor(() => expect(playhead()).toBe(1200));
    await user.click(screen.getByRole('button', { name: 'Forward 1 second' }));
    await waitFor(() => expect(from()).toBe(1201));
    expect(audio()).toHaveAttribute('preload', 'none');
    expect(play).not.toHaveBeenCalled();
    expect(playhead()).toBe(1201);
    // "Play 5 s" asks for it from the row's start, to play.
    const el = audio();
    await user.click(screen.getByRole('button', { name: 'Play 5 s' }));
    await waitFor(() => expect(play).toHaveBeenCalledTimes(1));
    expect(play.mock.contexts[0]).toBe(el);
    expect(from()).toBe(1200);
    expect(audio()).toHaveAttribute('preload', 'auto');
    play.mockRestore();
  });

  it('plays a transcode on the <audio> the click played, so Safari lets it play on', async () => {
    const play = vi.spyOn(HTMLMediaElement.prototype, 'play');
    const d = bookDetail();
    d.files = d.files.map((f) => ({ ...f, format: 'mka', codec: 'opus', direct_playable: false }));
    mockFetch(routes(d));
    const { container } = renderApp(URL);
    const user = await openEditor();
    await user.click(screen.getByRole('button', { name: 'Listen from chapter 2' }));
    await streamed(container);
    const audio = container.querySelector('audio')!;
    const from = () =>
      Number(new window.URL(audio.getAttribute('src')!, 'http://admin.test').searchParams.get('t'));
    // "Play 5 s" on a row just chosen plays its stream at once, in the click.
    fireEvent.click(screen.getByRole('button', { name: 'Play 5 s' }));
    expect(play).toHaveBeenCalledTimes(1);
    expect(from()).toBe(1200);
    // A move while it plays asks again from there and plays on the same element.
    Object.defineProperty(audio, 'paused', { value: false, configurable: true });
    Object.defineProperty(audio, 'currentTime', { value: 2, writable: true, configurable: true });
    await user.click(screen.getByRole('button', { name: 'Forward 1 second' }));
    await waitFor(() => expect(from()).toBe(1203));
    await waitFor(() => expect(play).toHaveBeenCalledTimes(2));
    expect(container.querySelector('audio')).toBe(audio);
    expect(play.mock.contexts[1]).toBe(audio);
    // Its load can begin after play() ran: the button says it plays.
    fireEvent.play(audio);
    fireEvent.loadStart(audio);
    expect(screen.getByRole('button', { name: 'Pause' })).toBeInTheDocument();
    play.mockRestore();
  });

  it('lets go of a paused transcode’s stream after a while, and plays on from there', async () => {
    const d = bookDetail();
    d.files = d.files.map((f) => ({ ...f, format: 'mka', codec: 'opus', direct_playable: false }));
    mockFetch(routes(d));
    const { container } = renderApp(URL);
    const user = await openEditor();
    await user.click(screen.getByRole('button', { name: 'Listen from chapter 2' }));
    await streamed(container);
    const audio = container.querySelector('audio')!;
    const from = () =>
      Number(new window.URL(audio.getAttribute('src')!, 'http://admin.test').searchParams.get('t'));
    const play = vi.spyOn(HTMLMediaElement.prototype, 'play');
    fireEvent.loadedMetadata(audio);
    // Paused before it played: nothing to let go of (it loaded nothing).
    vi.useFakeTimers();
    try {
      fireEvent.pause(audio);
      act(() => vi.advanceTimersByTime(10_000));
      expect(from()).toBe(1200);
      // Played 4.5 s, then paused: held a while, then let go of, at the playhead.
      Object.defineProperty(audio, 'currentTime', {
        value: 4.5,
        writable: true,
        configurable: true,
      });
      fireEvent.pause(audio);
      act(() => vi.advanceTimersByTime(9_000));
      expect(from()).toBe(1200);
      act(() => vi.advanceTimersByTime(1_000));
    } finally {
      vi.useRealTimers();
    }
    expect(from()).toBe(1204.5);
    expect(audio).toHaveAttribute('preload', 'none');
    expect(play).not.toHaveBeenCalled();
    Object.defineProperty(audio, 'currentTime', { value: 0, writable: true, configurable: true });
    await waitFor(() =>
      expect(screen.getByRole('slider', { name: /Position in/ })).toHaveValue('1204.5'),
    );
    // Play asks for it from there, in the click.
    await user.click(screen.getByRole('button', { name: 'Play' }));
    expect(play).toHaveBeenCalledTimes(1);
    expect(play.mock.contexts[0]).toBe(audio);
    play.mockRestore();
  });

  // A one-file book of MP3 in .mka, its remux made or not (remux_cached).
  const mkaDetail = (cached: boolean) => {
    const d = bookDetail({
      chapter_basis: { files: [{ path: '01.mka', size: 48_000_000, duration_ms: 3_000_000 }] },
    });
    d.files = d.files.map((f) => ({
      ...f,
      path: `${BOOK_PATH}/01.mka`,
      format: 'mka',
      codec: 'mp3',
      direct_playable: true,
      remux_cached: cached,
    }));
    d.chapters = d.chapters?.map((c) => ({ ...c, file_path: `${BOOK_PATH}/01.mka` }));
    return d;
  };

  it('asks the server to warm the remuxes only as the editor opens with a copy to come', async () => {
    const warms = (calls: MockRequest[]) =>
      calls.filter((c) => c.path === '/admin/libraries/1/book' && c.query.get('warm') === '1');
    const calls = mockFetch(routes(mkaDetail(false)));
    renderApp(URL);
    // A view of the page starts no copy.
    await screen.findByRole('button', { name: 'Edit chapters' });
    expect(calls.some((c) => c.path === '/admin/libraries/1/book')).toBe(true);
    expect(warms(calls)).toHaveLength(0);
    await openEditor();
    await waitFor(() => expect(warms(calls)).toHaveLength(1));
  });

  it('takes the warm read as the page, so a copy made by then plays as the remux', async () => {
    mockFetch(
      routes(mkaDetail(false), {
        'GET /admin/libraries/1/book': (req) => ({
          body: mkaDetail(req.query.get('warm') === '1'),
        }),
      }),
    );
    const { container } = renderApp(URL);
    const user = await openEditor();
    await user.click(screen.getByRole('button', { name: 'Listen from chapter 2' }));
    await waitFor(() =>
      expect(container.querySelector('audio')?.getAttribute('src')).not.toContain('transcode'),
    );
  });

  it('asks for no warm when every copy is made', async () => {
    const calls = mockFetch(routes(mkaDetail(true)));
    renderApp(URL);
    await openEditor();
    await act(() => new Promise((r) => setTimeout(r, 50)));
    expect(calls.filter((c) => c.query.get('warm') === '1')).toHaveLength(0);
  });

  it('transcodes an MP3 in .mka from the playhead until the page says its remux is made', async () => {
    // The server would answer a cold remux with a live transcode from the
    // file's start, which can't be seeked: the page says it is cold, whatever
    // length a browser reports for that stream (Safari's may be finite).
    let d = mkaDetail(false);
    const calls = mockFetch(routes(d, { 'GET /admin/libraries/1/book': () => ({ body: d }) }));
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
    try {
      const { container } = renderApp(URL);
      const user = await openEditor();
      await user.click(screen.getByRole('button', { name: 'Listen from chapter 2' }));
      await streamed(container);
      const audio = container.querySelector('audio')!;
      const query = () =>
        new window.URL(audio.getAttribute('src')!, 'http://admin.test').searchParams;
      expect(query().get('transcode')).toBe('1');
      expect(Number(query().get('t'))).toBe(1200);
      // A seek asks for it again from there.
      await user.click(screen.getByRole('button', { name: 'Forward 1 second' }));
      await waitFor(() => expect(Number(query().get('t'))).toBe(1201));

      // The page is read again while it waits for the copy.
      const reads = () => calls.filter((c) => c.path === '/admin/libraries/1/book').length;
      const before = reads();
      act(() => vi.advanceTimersByTime(5000));
      await waitFor(() => expect(reads()).toBe(before + 1));
      // Each such read asks the server to warm the copy.
      const last = calls.filter((c) => c.path === '/admin/libraries/1/book').at(-1);
      expect(last?.query.get('warm')).toBe('1');
      expect(query().get('transcode')).toBe('1');

      // Made while it plays: it plays on as the transcode.
      d = mkaDetail(true);
      Object.defineProperty(audio, 'paused', { value: false, configurable: true });
      fireEvent.play(audio);
      act(() => vi.advanceTimersByTime(5000));
      await waitFor(() => expect(reads()).toBe(before + 2));
      expect(query().get('transcode')).toBe('1');
      // Paused 2.5 s into the transcode: the copy takes over from there.
      Object.defineProperty(audio, 'paused', { value: true, configurable: true });
      Object.defineProperty(audio, 'currentTime', {
        value: 2.5,
        writable: true,
        configurable: true,
      });
      fireEvent.pause(audio);
      await waitFor(() => expect(query().get('transcode')).toBeNull());
      expect(container.querySelector('audio')).toBe(audio);
      fireEvent.loadedMetadata(audio);
      expect(audio.currentTime).toBe(1203.5);
      // No more reads once it is made.
      const after = reads();
      act(() => vi.advanceTimersByTime(15_000));
      expect(reads()).toBe(after);
    } finally {
      vi.useRealTimers();
    }
  });

  it('stops reading the page once it says the remux was refused, and keeps the transcode', async () => {
    let d = mkaDetail(false);
    const calls = mockFetch(routes(d, { 'GET /admin/libraries/1/book': () => ({ body: d }) }));
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
    try {
      const { container } = renderApp(URL);
      const user = await openEditor();
      await user.click(screen.getByRole('button', { name: 'Listen from chapter 2' }));
      await streamed(container);
      const audio = container.querySelector('audio')!;
      expect(audio.getAttribute('src')).toContain('transcode=1');
      const reads = () => calls.filter((c) => c.path === '/admin/libraries/1/book').length;
      const before = reads();
      // Refused: no remux, so no copy to wait for.
      d = mkaDetail(false);
      d.files = d.files.map((f) => ({ ...f, direct_playable: false, remux_cached: undefined }));
      act(() => vi.advanceTimersByTime(5000));
      await waitFor(() => expect(reads()).toBe(before + 1));
      // The answer lands (the page's query takes it) before the next tick.
      await act(() => new Promise((r) => setTimeout(r, 50)));
      act(() => vi.advanceTimersByTime(15_000));
      expect(reads()).toBe(before + 1);
      expect(audio.getAttribute('src')).toContain('transcode=1');
    } finally {
      vi.useRealTimers();
    }
  });

  it('keeps the page and the draft when a read for the remux fails', async () => {
    let fail = false;
    const d = mkaDetail(false);
    mockFetch(
      routes(d, {
        'GET /admin/libraries/1/book': () =>
          fail ? { status: 403, body: { error: 'forbidden' } } : { body: d },
      }),
    );
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
    try {
      const { container } = renderApp(URL);
      const user = await openEditor();
      await user.click(screen.getByRole('button', { name: 'Listen from chapter 2' }));
      await streamed(container);
      fail = true;
      for (let i = 0; i < 3; i++) {
        act(() => vi.advanceTimersByTime(5000));
        await act(() => new Promise((r) => setTimeout(r, 50)));
      }
      expect(screen.getByRole('button', { name: 'Save chapters' })).toBeInTheDocument();
    } finally {
      vi.useRealTimers();
    }
  });

  it('plays an MP3 in .mka as its remux once that is made', async () => {
    mockFetch(routes(mkaDetail(true)));
    const { container } = renderApp(URL);
    const user = await openEditor();
    await user.click(screen.getByRole('button', { name: 'Listen from chapter 2' }));
    await streamed(container);
    const audio = container.querySelector('audio')!;
    expect(audio.getAttribute('src')).not.toContain('transcode');
    fireEvent.loadedMetadata(audio);
    // A copy with a length: seeked in place, the row's start set at load.
    expect(audio.currentTime).toBe(1200);
    await user.click(screen.getByRole('button', { name: 'Forward 1 second' }));
    expect(audio.getAttribute('src')).not.toContain('transcode');
  });

  describe('a transcode that fails', () => {
    const opus = () => {
      const d = bookDetail();
      d.files = d.files.map((f) => ({
        ...f,
        format: 'mka',
        codec: 'opus',
        direct_playable: false,
      }));
      return d;
    };
    // jsdom plays nothing: the asks to load and play again are counted.
    let load: ReturnType<typeof vi.spyOn>;
    let play: ReturnType<typeof vi.spyOn>;
    beforeEach(() => {
      load = vi.spyOn(HTMLMediaElement.prototype, 'load');
      play = vi.spyOn(HTMLMediaElement.prototype, 'play').mockResolvedValue();
    });
    afterEach(() => vi.restoreAllMocks());

    async function listen(stream: MockRoute) {
      let n = 0;
      const calls = mockFetch(
        routes(opus(), {
          'POST /cast/token': () => ({
            status: 201,
            body: { token: `media-tok-${++n}`, expires_at: 'x' },
          }),
          'HEAD /libraries/1/stream': stream,
        }),
      );
      const { container } = renderApp(URL);
      const user = await openEditor();
      await user.click(screen.getByRole('button', { name: 'Listen from chapter 2' }));
      // The stream, with its token: the element is there before the mint
      // answers, and a test's first event must find the stream asked for.
      await waitFor(() =>
        expect(container.querySelector('audio')?.getAttribute('src')).toContain('media-tok-'),
      );
      load.mockClear();
      return {
        user,
        audio: container.querySelector('audio')!,
        mints: () => calls.filter((c) => c.path === '/cast/token').length,
        looks: () => calls.filter((c) => c.path === '/libraries/1/stream'),
      };
    }
    // Every transcode slot taken: 503 with a Retry-After (a short one, for the test).
    const busy = { status: 503, raw: '', headers: { 'Retry-After': '0.02' } };
    const busyLine = 'The server is busy. Trying again in a moment.';

    // A test that acts during a wait (a press, an event, a look at the page)
    // runs it on a fake clock, which moves only when the test moves it: a
    // loaded run gets there however late, the wait not yet run out. Faked
    // before the first play, so the stall's wait is the fake clock's too.
    // The look's answers settle on promises alone (settle); presses are
    // fireEvent's while it is fake (user-event and findBy wait on setTimeout).
    // The describe's afterEach puts the real clock back.
    const fakeClock = () => vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    const settle = () =>
      act(async () => {
        for (let i = 0; i < 20; i++) await Promise.resolve();
      });
    afterEach(() => vi.useRealTimers());

    it('waits and asks again when the server is too busy, with no new token', async () => {
      const { user, audio, mints, looks } = await listen(busy);
      fireEvent.play(audio);
      fireEvent.error(audio);
      await waitFor(() => expect(play).toHaveBeenCalledTimes(1));
      expect(load).toHaveBeenCalledTimes(1);
      // The look at the answer asked for the same stream, with the editor's token.
      expect(looks()).toHaveLength(1);
      // A HEAD: it starts no transcode of its own.
      expect(looks()[0].method).toBe('HEAD');
      expect(looks()[0].query.get('transcode')).toBe('1');
      expect(looks()[0].query.get('token')).toBe('media-tok-1');
      expect(mints()).toBe(1);
      expect(audio.getAttribute('src')).toContain('media-tok-1');
      expect(screen.queryByText(/The audio didn’t load/)).not.toBeInTheDocument();

      // Still busy after three asks: it gives up, the token still the first.
      fireEvent.error(audio);
      await waitFor(() => expect(play).toHaveBeenCalledTimes(2));
      fireEvent.error(audio);
      await waitFor(() => expect(play).toHaveBeenCalledTimes(3));
      fireEvent.error(audio);
      expect(await screen.findByText(/The audio didn’t load/)).toBeInTheDocument();
      expect(play).toHaveBeenCalledTimes(3);
      expect(mints()).toBe(1);

      // Another row has its own tries.
      await user.click(screen.getByRole('button', { name: 'Listen from chapter 1' }));
      await waitFor(() =>
        expect(screen.queryByText(/The audio didn’t load/)).not.toBeInTheDocument(),
      );
      expect(screen.getByRole('button', { name: 'Play' })).toBeEnabled();
    });

    it('asks a busy stream paused before it failed for again without playing it', async () => {
      const { audio } = await listen(busy);
      fireEvent.play(audio);
      fireEvent.pause(audio);
      fireEvent.error(audio);
      await waitFor(() => expect(load).toHaveBeenCalledTimes(1));
      await new Promise((r) => setTimeout(r, 50));
      expect(play).not.toHaveBeenCalled();
    });

    // An <audio> event at a given time (its timeStamp, ms), the element's
    // error set first when `failed` (WebKit keeps it while a busy stream waits).
    const at = (audio: HTMLAudioElement, type: string, ms: number) => {
      const ev = new Event(type);
      Object.defineProperty(ev, 'timeStamp', { value: ms });
      act(() => {
        audio.dispatchEvent(ev);
      });
    };
    const failed = (audio: HTMLAudioElement) =>
      Object.defineProperty(audio, 'error', { value: { code: 2 }, configurable: true });

    it('does not play a busy stream paused while it waits, though WebKit keeps the error', async () => {
      const { audio } = await listen({ ...busy, headers: { 'Retry-After': '0.2' } });
      Object.defineProperty(audio, 'paused', { value: false, configurable: true });
      fakeClock();
      fireEvent.play(audio);
      failed(audio);
      at(audio, 'error', 1000);
      await settle();
      expect(screen.getByText(busyLine)).toBeInTheDocument();
      // A headset's pause, well after the failure: the error is still set.
      Object.defineProperty(audio, 'paused', { value: true, configurable: true });
      at(audio, 'pause', 3000);
      act(() => vi.advanceTimersByTime(300)); // the wait runs out
      expect(load).toHaveBeenCalledTimes(1);
      expect(play).not.toHaveBeenCalled();
    });

    // Chrome's: the element paused as its load fails.
    it.each([
      ['before its error', 1000, 1001],
      ['just after its error', 1100, 1000],
    ])('counts the pause a failed load makes as none: %s', async (_, pauseAt, errorAt) => {
      const { audio } = await listen({ ...busy, headers: { 'Retry-After': '0.02' } });
      fireEvent.play(audio);
      failed(audio);
      if (pauseAt < errorAt) at(audio, 'pause', pauseAt);
      at(audio, 'error', errorAt);
      if (pauseAt > errorAt) at(audio, 'pause', pauseAt);
      await waitFor(() => expect(play).toHaveBeenCalledTimes(1));
    });

    it('drops the wait when another row is chosen', async () => {
      const { audio, looks } = await listen({ ...busy, headers: { 'Retry-After': '0.2' } });
      fakeClock();
      fireEvent.error(audio);
      await settle();
      expect(looks()).toHaveLength(1);
      expect(screen.getByText(busyLine)).toBeInTheDocument();
      fireEvent.click(screen.getByRole('button', { name: 'Listen from chapter 1' }));
      expect(screen.queryByText(/The server is busy/)).not.toBeInTheDocument();
      act(() => vi.advanceTimersByTime(300));
      expect(play).not.toHaveBeenCalled();
    });

    it('says the server is busy while it waits to ask again', async () => {
      const { audio } = await listen({ ...busy, headers: { 'Retry-After': '0.2' } });
      expect(screen.queryByText(busyLine)).not.toBeInTheDocument();
      fakeClock();
      fireEvent.play(audio);
      fireEvent.error(audio);
      await settle();
      expect(screen.getByText(busyLine)).toBeInTheDocument();
      // Asked again: the line goes.
      act(() => vi.advanceTimersByTime(300));
      expect(play).toHaveBeenCalledTimes(1);
      expect(screen.queryByText(busyLine)).not.toBeInTheDocument();
    });

    it('stops waiting when a move asks for the stream again from elsewhere', async () => {
      const { audio } = await listen({ ...busy, headers: { 'Retry-After': '0.4' } });
      const from = () =>
        Number(
          new window.URL(audio.getAttribute('src')!, 'http://admin.test').searchParams.get('t'),
        );
      fakeClock();
      fireEvent.error(audio);
      await settle();
      expect(screen.getByText(busyLine)).toBeInTheDocument();
      load.mockClear();
      fireEvent.click(screen.getByRole('button', { name: 'Forward 1 second' }));
      act(() => vi.advanceTimersByTime(300)); // the seek's ask (RESTREAM_MS), inside the wait
      await settle();
      expect(from()).toBe(1201);
      // The wait was the old stream's: it ends with the ask, and the new
      // stream is not loaded again when it would have run out.
      expect(screen.queryByText(/The server is busy/)).not.toBeInTheDocument();
      act(() => vi.advanceTimersByTime(500));
      expect(load).not.toHaveBeenCalled();
    });

    // Into a busy stream's wait, on the fake clock.
    async function busyOnAFakeClock(audio: HTMLAudioElement) {
      fakeClock();
      fireEvent.play(audio);
      fireEvent.error(audio);
      await settle();
      expect(screen.getByText(busyLine)).toBeInTheDocument();
    }

    it('stops the wait when its button is pressed, and asks at once on Play after', async () => {
      const { user, audio, looks } = await listen({ ...busy, headers: { 'Retry-After': '0.2' } });
      // Past the look's answer, into the wait: the button stops it, no Play.
      await busyOnAFakeClock(audio);
      expect(looks()).toHaveLength(1);
      expect(screen.queryByRole('button', { name: 'Play' })).not.toBeInTheDocument();
      const pause = vi.spyOn(HTMLMediaElement.prototype, 'pause');
      fireEvent.click(screen.getByRole('button', { name: 'Stop trying again' }));
      expect(pause).toHaveBeenCalledTimes(1);
      expect(screen.queryByText(/The server is busy/)).not.toBeInTheDocument();
      // The wait ran out with nothing asked again.
      act(() => vi.advanceTimersByTime(300));
      expect(load).not.toHaveBeenCalled();
      expect(play).not.toHaveBeenCalled();
      vi.useRealTimers();
      // Play asks again at once.
      await user.click(screen.getByRole('button', { name: 'Play' }));
      expect(load).toHaveBeenCalledTimes(1);
      expect(play).toHaveBeenCalledTimes(1);
    });

    it('asks at once on Play 5 s during the wait, and the wait asks nothing after', async () => {
      const { audio } = await listen({ ...busy, headers: { 'Retry-After': '0.2' } });
      await busyOnAFakeClock(audio);
      fireEvent.click(screen.getByRole('button', { name: 'Play 5 s' }));
      expect(screen.queryByText(/The server is busy/)).not.toBeInTheDocument();
      expect(load).toHaveBeenCalledTimes(1);
      expect(play).toHaveBeenCalledTimes(1);
      // The wait's own ask again would load the element over the preview.
      act(() => vi.advanceTimersByTime(300));
      expect(load).toHaveBeenCalledTimes(1);
      expect(play).toHaveBeenCalledTimes(1);
    });

    it('asks at once on Play 5 s during the look at a failed load, whose late answer starts no wait', async () => {
      let answer!: (reply: MockReply) => void;
      const { audio, looks } = await listen(
        () => new Promise<MockReply>((resolve) => (answer = resolve)),
      );
      fakeClock();
      fireEvent.play(audio);
      fireEvent.error(audio);
      await settle();
      expect(looks()).toHaveLength(1);
      fireEvent.click(screen.getByRole('button', { name: 'Play 5 s' }));
      expect(load).toHaveBeenCalledTimes(1);
      expect(play).toHaveBeenCalledTimes(1);
      // The look was the failed load's: its busy answer waits for nothing and
      // asks nothing again over the preview.
      answer({ ...busy, headers: { 'Retry-After': '0.05' } });
      await settle();
      act(() => vi.advanceTimersByTime(200));
      expect(screen.queryByText(/The server is busy/)).not.toBeInTheDocument();
      expect(load).toHaveBeenCalledTimes(1);
      expect(play).toHaveBeenCalledTimes(1);
    });

    it('gives Play 5 s during the wait its own tries', async () => {
      let n = 0;
      const { user, audio } = await listen(() => ({
        ...busy,
        headers: { 'Retry-After': ++n < 3 ? '0.02' : '5' },
      }));
      fireEvent.play(audio);
      fireEvent.error(audio);
      await waitFor(() => expect(play).toHaveBeenCalledTimes(1));
      fireEvent.error(audio);
      await waitFor(() => expect(play).toHaveBeenCalledTimes(2));
      // The third wait, the row's last try: busy once more after it gives up.
      fireEvent.error(audio);
      await screen.findByText('The server is busy. Trying again in a moment.');
      await user.click(screen.getByRole('button', { name: 'Play 5 s' }));
      expect(play).toHaveBeenCalledTimes(3);
      // Busy again, asked before the wait was over: waited for, not given up on.
      fireEvent.error(audio);
      await screen.findByText('The server is busy. Trying again in a moment.');
      expect(screen.queryByText(/The audio didn’t load/)).not.toBeInTheDocument();
      await user.click(screen.getByRole('button', { name: 'Stop trying again' }));
    });

    it('loads again on Play after a failed load that left the audio unpaused', async () => {
      // Chrome leaves paused false after a failed load: Play must not pause it.
      const { user, audio } = await listen({ ...busy, headers: { 'Retry-After': '0.4' } });
      Object.defineProperty(audio, 'paused', { value: false, configurable: true });
      fakeClock();
      fireEvent.play(audio);
      expect(screen.getByRole('button', { name: 'Pause' })).toBeInTheDocument();
      fireEvent.error(audio);
      await settle();
      expect(screen.getByText(busyLine)).toBeInTheDocument();
      fireEvent.click(screen.getByRole('button', { name: 'Stop trying again' }));
      vi.useRealTimers();
      const pause = vi.spyOn(HTMLMediaElement.prototype, 'pause');
      await user.click(screen.getByRole('button', { name: 'Play' }));
      expect(pause).not.toHaveBeenCalled();
      expect(load).toHaveBeenCalledTimes(1);
      expect(play).toHaveBeenCalledTimes(1);
      // Its load has begun: the button pauses it again.
      fireEvent.loadStart(audio);
      await user.click(screen.getByRole('button', { name: 'Pause' }));
      expect(pause).toHaveBeenCalledTimes(1);
    });

    it('says it can’t play when a new token fails, asking nothing with the old one', async () => {
      let n = 0;
      const calls = mockFetch(
        routes(opus(), {
          'POST /cast/token': () =>
            ++n === 1
              ? { status: 201, body: { token: 'media-tok-1', expires_at: 'x' } }
              : { status: 500, body: { error: 'down' } },
          'HEAD /libraries/1/stream': { status: 401, body: { error: 'unauthorized' } },
        }),
      );
      const { container } = renderApp(URL);
      const user = await openEditor();
      await user.click(screen.getByRole('button', { name: 'Listen from chapter 2' }));
      await streamed(container);
      const audio = container.querySelector('audio')!;
      // Playing: its stream would load at once if it came back.
      await user.click(screen.getByRole('button', { name: 'Play 5 s' }));
      fireEvent.error(audio);
      expect(await screen.findByText(/The audio didn’t load/)).toBeInTheDocument();
      expect(n).toBe(2);
      expect(audio.getAttribute('src')).toBeNull();
      expect(calls.filter((c) => c.path === '/libraries/1/stream')).toHaveLength(1);
    });

    it('mints a new token when the answer is not a busy one', async () => {
      const { audio, mints } = await listen({ status: 401, body: { error: 'unauthorized' } });
      fireEvent.error(audio);
      await waitFor(() => expect(audio.getAttribute('src')).toContain('media-tok-2'));
      expect(mints()).toBe(2);
      expect(play).not.toHaveBeenCalled();
      // The new token fails too: it can't play, and no third mint.
      fireEvent.error(audio);
      expect(await screen.findByText(/The audio didn’t load/)).toBeInTheDocument();
      expect(mints()).toBe(2);
    });

    it('plays on from where it was after a new token, when it was playing', async () => {
      const { audio, mints } = await listen({ status: 401, body: { error: 'unauthorized' } });
      const query = () =>
        new window.URL(audio.getAttribute('src')!, 'http://admin.test').searchParams;
      Object.defineProperty(audio, 'paused', { value: false, configurable: true });
      Object.defineProperty(audio, 'currentTime', {
        value: 30,
        writable: true,
        configurable: true,
      });
      fireEvent.error(audio);
      await waitFor(() => expect(query().get('token')).toBe('media-tok-2'));
      expect(mints()).toBe(2);
      expect(Number(query().get('t'))).toBe(1230);
      await waitFor(() => expect(play).toHaveBeenCalledTimes(1));
      expect(play.mock.contexts[0]).toBe(audio);
    });

    // Chrome sets paused after a failed load: the Play pressed before it says
    // the audio is to play on with the new token, not the element.
    const srcParams = (audio: HTMLAudioElement) =>
      new window.URL(audio.getAttribute('src')!, 'http://admin.test').searchParams;

    it('plays on with a new token after Play, though the failed load left it paused', async () => {
      const { user, audio } = await listen({ status: 401, body: { error: 'unauthorized' } });
      await user.click(screen.getByRole('button', { name: 'Play' }));
      expect(play).toHaveBeenCalledTimes(1);
      expect(audio.paused).toBe(true);
      fireEvent.error(audio);
      await waitFor(() => expect(srcParams(audio).get('token')).toBe('media-tok-2'));
      await waitFor(() => expect(play).toHaveBeenCalledTimes(2));
    });

    it('plays on with a new token after a move made while playing', async () => {
      const { user, audio } = await listen({ status: 401, body: { error: 'unauthorized' } });
      fireEvent.play(audio);
      await user.click(screen.getByRole('button', { name: 'Forward 1 second' }));
      await waitFor(() => expect(play).toHaveBeenCalledTimes(1));
      expect(Number(srcParams(audio).get('t'))).toBe(1201);
      fireEvent.error(audio);
      await waitFor(() => expect(srcParams(audio).get('token')).toBe('media-tok-2'));
      expect(Number(srcParams(audio).get('t'))).toBe(1201);
      await waitFor(() => expect(play).toHaveBeenCalledTimes(2));
    });

    it('does not play with a new token once paused', async () => {
      const { audio } = await listen({ status: 401, body: { error: 'unauthorized' } });
      fireEvent.play(audio);
      fireEvent.pause(audio);
      fireEvent.error(audio);
      await waitFor(() => expect(srcParams(audio).get('token')).toBe('media-tok-2'));
      await new Promise((r) => setTimeout(r, 50));
      expect(play).not.toHaveBeenCalled();
    });

    // Safari asks for a stream more than once: with one slot free, a later ask
    // is a 503 and the <audio> waits on, unpaused, with no error.
    async function stall(stream: MockRoute) {
      const got = await listen(stream);
      Object.defineProperty(got.audio, 'paused', { value: false, configurable: true });
      vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
      fireEvent.play(got.audio);
      return {
        ...got,
        // Past the stall's wait (counting nothing asked before its end), then
        // real timers for the look and the retry, or the fake clock kept for a
        // test that acts during the busy answer's wait (fake).
        wait: (before?: () => void, fake = false) => {
          act(() => vi.advanceTimersByTime(5900));
          before?.();
          act(() => vi.advanceTimersByTime(100));
          if (!fake) vi.useRealTimers();
        },
      };
    }
    it('says the server is busy and asks again when a stream that started nothing was a 503', async () => {
      const { audio, looks, mints, wait } = await stall({
        ...busy,
        headers: { 'Retry-After': '0.2' },
      });
      wait(() => expect(looks()).toHaveLength(0), true);
      await settle();
      expect(screen.getByText(busyLine)).toBeInTheDocument();
      expect(looks()).toHaveLength(1);
      // The wait can be stopped meanwhile.
      expect(screen.getByRole('button', { name: 'Stop trying again' })).toBeEnabled();
      act(() => vi.advanceTimersByTime(300));
      expect(play).toHaveBeenCalledTimes(1);
      expect(load).toHaveBeenCalledTimes(1);
      expect(play.mock.contexts[0]).toBe(audio);
      expect(mints()).toBe(1);
    });

    it('plays on when a stalled stream starts while it waits to be asked for again', async () => {
      const { audio, looks, wait } = await stall({ ...busy, headers: { 'Retry-After': '0.2' } });
      wait(undefined, true);
      await settle();
      expect(screen.getByText(busyLine)).toBeInTheDocument();
      expect(looks()).toHaveLength(1);
      fireEvent.playing(audio);
      expect(screen.queryByText(/The server is busy/)).not.toBeInTheDocument();
      expect(screen.getByRole('button', { name: 'Pause' })).toBeEnabled();
      act(() => vi.advanceTimersByTime(300));
      expect(load).not.toHaveBeenCalled();
      expect(play).not.toHaveBeenCalled();
    });

    it('lets a stream be that has started', async () => {
      const started = await stall(busy);
      fireEvent.playing(started.audio);
      started.wait();
      await new Promise((r) => setTimeout(r, 50));
      expect(started.looks()).toHaveLength(0);
    });

    // WebKit, one slot free: it drops the stream it was given and waits on
    // with none, the look answering that the server would play it now.
    it('loads a stalled stream again once when the server has a slot, then says it didn’t load', async () => {
      const { audio, looks, mints, wait } = await stall({ status: 200, raw: '' });
      const pause = vi.spyOn(HTMLMediaElement.prototype, 'pause');
      wait();
      await waitFor(() => expect(play).toHaveBeenCalledTimes(1));
      expect(looks()).toHaveLength(1);
      expect(load).toHaveBeenCalledTimes(1);
      expect(play.mock.contexts[0]).toBe(audio);
      expect(screen.queryByText(/The server is busy/)).not.toBeInTheDocument();
      expect(screen.queryByText(/The audio didn’t load/)).not.toBeInTheDocument();

      // Loaded again, it stalls again: it didn't load, and lets go of the stream.
      vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
      fireEvent.loadStart(audio);
      fireEvent.play(audio);
      act(() => vi.advanceTimersByTime(6000));
      vi.useRealTimers();
      expect(await screen.findByText(/The audio didn’t load/)).toBeInTheDocument();
      expect(looks()).toHaveLength(2);
      expect(load).toHaveBeenCalledTimes(1);
      expect(play).toHaveBeenCalledTimes(1);
      expect(pause).toHaveBeenCalled();
      expect(mints()).toBe(1);
    });

    it('loads a stalled stream again once for each ask: a seek since gets its own', async () => {
      const { user, audio, looks, wait } = await stall({ status: 200, raw: '' });
      wait();
      await waitFor(() => expect(load).toHaveBeenCalledTimes(1));

      // A seek asks for a new stream, which stalls too: loaded again, not given up on.
      await user.click(screen.getByRole('button', { name: 'Forward 1 second' }));
      await waitFor(() => expect(Number(srcParams(audio).get('t'))).toBe(1201));
      vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
      fireEvent.loadStart(audio);
      fireEvent.play(audio);
      act(() => vi.advanceTimersByTime(6000));
      vi.useRealTimers();
      await waitFor(() => expect(load).toHaveBeenCalledTimes(2));
      expect(looks()).toHaveLength(2);
      expect(screen.queryByText(/The audio didn’t load/)).not.toBeInTheDocument();
    });

    it('lets a stalled stream be that starts while the server is asked about it', async () => {
      let answer: (r: MockReply) => void = () => {};
      const { audio, looks, wait } = await stall(() => new Promise<MockReply>((r) => (answer = r)));
      wait();
      await waitFor(() => expect(looks()).toHaveLength(1));
      Object.defineProperty(audio, 'readyState', { value: 4, configurable: true });
      answer({ status: 200, raw: '' });
      await new Promise((r) => setTimeout(r, 50));
      expect(load).not.toHaveBeenCalled();
      expect(play).not.toHaveBeenCalled();
      expect(screen.queryByText(/The audio didn’t load/)).not.toBeInTheDocument();
    });

    // A slow transcode, its own ask holding the slot: asked for again, it would
    // be ended (the new ask takes its slot over), so it is waited for while it
    // sends more between looks, and loaded again only once it sends no more.
    it('waits on a slow stream while its transcode sends more, and loads it again once it stops', async () => {
      const sent = ['1000', '5000', '5000', '100'];
      let n = 0;
      const { audio, looks } = await stall(() => ({
        status: 200,
        raw: '',
        headers: { 'X-AudioSilo-Transcode-Sent': sent[n++] },
      }));
      act(() => vi.advanceTimersByTime(6000));
      await settle();
      expect(looks()).toHaveLength(1);
      // Still sending: looked at again after another wait, not asked for again.
      act(() => vi.advanceTimersByTime(6000));
      await settle();
      expect(looks()).toHaveLength(2);
      expect(load).not.toHaveBeenCalled();
      expect(play).not.toHaveBeenCalled();
      expect(screen.queryByText(/The audio didn’t load/)).not.toBeInTheDocument();

      // It sent nothing more: dropped as far as the <audio> goes, loaded again.
      act(() => vi.advanceTimersByTime(6000));
      await settle();
      expect(looks()).toHaveLength(3);
      expect(load).toHaveBeenCalledTimes(1);
      expect(play).toHaveBeenCalledTimes(1);

      // The stream loaded again is its own: its first look waits for it.
      fireEvent.loadStart(audio);
      fireEvent.play(audio);
      act(() => vi.advanceTimersByTime(6000));
      await settle();
      expect(looks()).toHaveLength(4);
      expect(load).toHaveBeenCalledTimes(1);
      expect(screen.queryByText(/The audio didn’t load/)).not.toBeInTheDocument();
    });

    // A busy answer's wait loads the stream again with no new ask: what the
    // last stream had sent is not the new one's, whose first look waits for it.
    it('waits on a stream loaded again after a busy answer, whatever the last one had sent', async () => {
      const replies: MockReply[] = [
        { status: 200, raw: '', headers: { 'X-AudioSilo-Transcode-Sent': '5000' } },
        { status: 503, raw: '', headers: { 'Retry-After': '0.2' } },
        { status: 200, raw: '', headers: { 'X-AudioSilo-Transcode-Sent': '100' } },
      ];
      let n = 0;
      const { audio, looks } = await stall(() => replies[n++]);
      act(() => vi.advanceTimersByTime(6000));
      await settle();
      expect(looks()).toHaveLength(1);
      // Its transcode gone and every slot taken: loaded again after the wait.
      act(() => vi.advanceTimersByTime(6000));
      await settle();
      expect(looks()).toHaveLength(2);
      act(() => vi.advanceTimersByTime(200));
      expect(load).toHaveBeenCalledTimes(1);
      expect(play).toHaveBeenCalledTimes(1);

      // The new stream has sent less than the last one had: still its first look.
      fireEvent.loadStart(audio);
      fireEvent.play(audio);
      act(() => vi.advanceTimersByTime(6000));
      await settle();
      expect(looks()).toHaveLength(3);
      expect(load).toHaveBeenCalledTimes(1);
      expect(play).toHaveBeenCalledTimes(1);
      expect(screen.queryByText(/The audio didn’t load/)).not.toBeInTheDocument();
    });

    // Chrome, after a stall's look said busy: the element's load fails, its
    // pause before the error event. That pause is the failure's, not the user's.
    it('counts a failed load’s pause as none after a stall’s look said busy', async () => {
      const { audio, wait } = await stall({ ...busy, headers: { 'Retry-After': '0.2' } });
      wait();
      await screen.findByText('The server is busy. Trying again in a moment.');
      Object.defineProperty(audio, 'paused', { value: true, configurable: true });
      failed(audio);
      at(audio, 'pause', 9000);
      at(audio, 'error', 9001);
      await waitFor(() => expect(play).toHaveBeenCalledTimes(1));
      expect(play.mock.contexts[0]).toBe(audio);
    });
  });

  it('plays the stored file when the server can’t transcode', async () => {
    const d = bookDetail();
    d.files = d.files.map((f) => ({ ...f, format: 'mka', codec: 'opus', direct_playable: false }));
    mockFetch(
      routes(d, {
        'GET /server': {
          body: { ...serverInfo, capabilities: { ...serverInfo.capabilities, transcode: false } },
        },
      }),
    );
    const { container } = renderApp(URL);
    const user = await openEditor();
    await user.click(screen.getByRole('button', { name: 'Listen from chapter 2' }));
    await streamed(container);
    const src = container.querySelector('audio')!.getAttribute('src')!;
    expect(src).not.toContain('transcode');
  });

  it('reads the book’s jobs again after a save: it cancelled a queued detection', async () => {
    const calls = mockFetch(
      routes(bookDetail(), {
        'GET /server': {
          body: { ...serverInfo, capabilities: { ...serverInfo.capabilities, bridge: true } },
        },
        'GET /admin/bridge/jobs': { body: { jobs: [], next_before: 0 } },
        'PUT /admin/libraries/1/book/chapters': { body: bookDetail() },
      }),
    );
    renderApp(URL);
    const user = await openEditor();
    const jobReads = () => calls.filter((c) => c.path === '/admin/bridge/jobs').length;
    await waitFor(() => expect(jobReads()).toBe(1));
    await user.type(screen.getByRole('textbox', { name: 'Title of chapter 2' }), '!');
    await user.click(screen.getByRole('button', { name: 'Save chapters' }));
    await waitFor(() => expect(puts(calls)).toHaveLength(1));
    await waitFor(() => expect(jobReads()).toBe(2));
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
