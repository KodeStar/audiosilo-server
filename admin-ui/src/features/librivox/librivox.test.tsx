import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { ApiError } from '@/api/client';
import { setToken } from '@/api/token';
import type {
  BridgeJob,
  BridgeStatus,
  LibrivoxHit,
  LibrivoxRecording,
  LibrivoxWork,
} from '@/api/types';
import en from '@/i18n/locales/en.json';
import { mockFetch, type MockRoute } from '@/test/fetch-mock';
import { serverInfo } from '@/test/fixtures';
import { renderApp } from '@/test/render-app';
import { signedInRoutes } from '@/test/routes';
import { JOBS_POLL_OPEN_MS } from '@/features/bridge/bridge-model';
import {
  DONE_RECHECK_MS,
  FETCH_CODES,
  addBlock,
  bridgeNote,
  fetchReason,
  jobGone,
  jobPollMs,
  librivoxProject,
} from './librivox-model';

// Library > LibriVox (LibriVox L6): search the community database's LibriVox
// works, see a work's recordings, and add one through the Bridge.

const bridgeStatus: BridgeStatus = {
  paired: true,
  paired_at: '2026-10-15T09:00:00Z',
  paired_by: { id: 1, username: 'admin' },
  pairing: null,
  online: true,
  heartbeat_at: new Date().toISOString(),
  version: '0.7.0',
  kinds: ['bridge.verify', 'librivox.fetch'],
  features: ['bridge', 'librivox'],
  libraries: [],
  workers: [],
  ui_url: 'http://192.168.1.20:8090/',
};

const hit: LibrivoxHit = {
  work_id: 'johnny-crows-garden',
  title: "Johnny Crow's Garden",
  authors: [{ id: 'l-leslie-brooke', name: 'L. Leslie Brooke' }],
  language: 'en',
  release_date: '2009',
};

function recording(over: Partial<LibrivoxRecording> = {}): LibrivoxRecording {
  return {
    recording_id: 'lv192',
    librivox_id: 192,
    narrators: [{ id: 'kara-shallenberg', name: 'Kara Shallenberg' }],
    runtime_min: 12,
    chapter_count: 3,
    cover_url: '',
    url: 'https://librivox.org/johnny-crows-garden-by-l-leslie-brooke/',
    in_library: [],
    job: null,
    ...over,
  };
}

function work(recordings: LibrivoxRecording[] = [recording()]): LibrivoxWork {
  return {
    work: { id: hit.work_id, title: hit.title, authors: hit.authors, language: 'en' },
    recordings,
  };
}

function job(over: Partial<BridgeJob> = {}): BridgeJob {
  return {
    id: 41,
    kind: 'librivox.fetch',
    library_id: 1,
    path: null,
    params: { librivox_id: 192, work_id: hit.work_id, recording_id: 'lv192', library_id: 1 },
    state: 'queued',
    progress: {},
    result: {},
    error: '',
    requested_by: { id: 1, username: 'chris' },
    priority: 0,
    attempts: 0,
    lease_until: null,
    cancel_requested: false,
    created_at: '2026-11-13T09:00:00Z',
    updated_at: '2026-11-13T09:00:00Z',
    claimed_at: null,
    finished_at: null,
    ...over,
  };
}

function routes(over: Record<string, MockRoute> = {}) {
  return signedInRoutes({
    'GET /admin/bridge': { body: bridgeStatus },
    'GET /admin/bridge/jobs': { body: { jobs: [], next_before: 0 } },
    'GET /admin/librivox/search': { body: { results: [hit] } },
    'GET /admin/librivox/work': { body: work() },
    ...over,
  });
}

/** Opens the page, searches for "johnny" and opens the hit's work. */
async function openWork() {
  const user = userEvent.setup();
  renderApp('/library/librivox');
  await user.type(await screen.findByRole('textbox', { name: 'Title or author' }), 'johnny');
  await user.click(await screen.findByRole('button', { name: /Johnny Crow's Garden/ }));
  const list = await screen.findByRole('list', { name: 'Recordings' });
  return { user, list };
}

beforeEach(() => setToken('stored'));

describe('librivox model', () => {
  it('blocks Add until a paired Bridge that runs librivox.fetch has reported', () => {
    expect(addBlock(undefined, false)).toBe('loading');
    expect(addBlock(undefined, true)).toBe('error');
    expect(addBlock({ paired: false, features: [] }, false)).toBe('unpaired');
    expect(addBlock({ paired: true, features: ['bridge', 'audible'] }, false)).toBe('unsupported');
    expect(addBlock({ paired: true, features: ['librivox'] }, false)).toBeUndefined();
    // Offline is no block (the job waits), only a note.
    expect(bridgeNote({ ...bridgeStatus, online: false }, false)).toBe('offline');
    expect(bridgeNote(bridgeStatus, false)).toBeUndefined();
    expect(bridgeNote({ ...bridgeStatus, paired: false, online: false }, false)).toBe('unpaired');
  });

  it("names a fetch's known failure code, from its park, result or error", () => {
    expect(
      fetchReason(job({ state: 'needs_attention', progress: { park: { code: 'disk_full' } } })),
    ).toBe('librivox.code.disk_full');
    expect(fetchReason(job({ state: 'failed', result: { code: 'exists' } }))).toBe(
      'librivox.code.exists',
    );
    expect(
      fetchReason(job({ state: 'failed', error: 'not_writable: read-only file system' })),
    ).toBe('librivox.code.not_writable');
    // Unknown codes, open or done jobs, and other kinds name nothing.
    expect(fetchReason(job({ state: 'failed', result: { code: 'mystery' } }))).toBeUndefined();
    expect(fetchReason(job({ state: 'failed', error: 'exists_not' }))).toBeUndefined();
    expect(fetchReason(job({ state: 'running', error: 'exists' }))).toBeUndefined();
    expect(
      fetchReason(job({ kind: 'audible.import', state: 'failed', error: 'exists' })),
    ).toBeUndefined();
    // Every code has its words.
    const strings = en as Record<string, string>;
    for (const code of FETCH_CODES) expect(strings[`librivox.code.${code}`]).toBeTruthy();
  });

  it('reads the project from params and polls only an open job', () => {
    expect(librivoxProject(job())).toBe(192);
    expect(librivoxProject(job({ params: { librivox_id: '192' } }))).toBeUndefined();
    expect(librivoxProject(job({ params: { librivox_id: 0 } }))).toBeUndefined();
    expect(librivoxProject(job({ kind: 'audible.import' }))).toBeUndefined();
    expect(jobPollMs(undefined)).toBe(false);
    expect(jobPollMs(job({ state: 'running' }))).toBe(JOBS_POLL_OPEN_MS);
    expect(jobPollMs(job({ state: 'done' }))).toBe(false);
  });

  it('tells a job that is gone from one that could not be read', () => {
    expect(jobGone(new ApiError(404, 'job not found'))).toBe(true);
    expect(jobGone(new ApiError(502, 'bad gateway'))).toBe(false);
    expect(jobGone(new Error('offline'))).toBe(false);
    expect(jobGone(null)).toBe(false);
  });
});

describe('Library > LibriVox', () => {
  it('says community metadata is off instead of searching', async () => {
    const calls = mockFetch(
      routes({
        'GET /server': {
          body: { ...serverInfo, capabilities: { ...serverInfo.capabilities, metadata: false } },
        },
      }),
    );
    renderApp('/library/librivox');
    expect(await screen.findByText('Community metadata is off')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Turn it on in Metadata settings' })).toHaveAttribute(
      'href',
      expect.stringContaining('topic=metadata'),
    );
    expect(screen.queryByRole('textbox', { name: 'Title or author' })).not.toBeInTheDocument();
    expect(calls.some((c) => c.path.startsWith('/admin/librivox'))).toBe(false);
  });

  it('searches, opens a work and adds a recording to the chosen library', async () => {
    const calls = mockFetch(
      routes({
        'POST /admin/librivox/fetch': { status: 202, body: { job: job() } },
        'GET /admin/bridge/jobs/41': { body: job() },
      }),
    );
    const { user, list } = await openWork();
    const search = calls.find((c) => c.path === '/admin/librivox/search');
    expect(search?.query.get('q')).toBe('johnny');
    expect(calls.find((c) => c.path === '/admin/librivox/work')?.query.get('id')).toBe(hit.work_id);

    const row = within(list).getAllByRole('listitem')[0];
    expect(within(row).getByText('Read by Kara Shallenberg')).toBeInTheDocument();
    expect(within(row).getByText(/3 sections · Project 192/)).toBeInTheDocument();
    expect(within(row).getByRole('link', { name: /LibriVox page/ })).toHaveAttribute(
      'href',
      recording().url,
    );
    // Two book libraries: a picker, then Add.
    await user.selectOptions(within(row).getByRole('combobox', { name: 'Library' }), 'Kids');
    await user.click(within(row).getByRole('button', { name: 'Add' }));
    await waitFor(() =>
      expect(
        calls.find((c) => c.method === 'POST' && c.path === '/admin/librivox/fetch')?.body,
      ).toEqual({
        library_id: 2,
        librivox_id: 192,
      }),
    );
    // The queued job shows, and Add waits for it.
    expect(await within(row).findByText(/Queued/i)).toBeInTheDocument();
    expect(within(row).getByRole('button', { name: 'Add' })).toBeDisabled();
  });

  it('links a recording that is already in a library to its book, with no Add', async () => {
    mockFetch(
      routes({
        'GET /admin/librivox/work': {
          body: work([
            recording({
              in_library: [{ library_id: 1, path: "L. Leslie Brooke/Johnny Crow's Garden" }],
            }),
          ]),
        },
      }),
    );
    const { list } = await openWork();
    const link = within(list).getByRole('link', { name: 'In library' });
    expect(link).toHaveAttribute('href', expect.stringContaining('/library/book'));
    expect(link).toHaveAttribute('href', expect.stringContaining('library=1'));
    expect(within(list).queryByRole('button', { name: /Add/ })).not.toBeInTheDocument();
  });

  it.each([
    [{ ...bridgeStatus, paired: false, online: false, features: [] }, /needs a paired Bridge/],
    [{ ...bridgeStatus, features: ['bridge', 'audible'] }, /can't add LibriVox books yet/],
  ] as const)('disables Add without a Bridge that runs it: %o', async (status, note) => {
    const calls = mockFetch(routes({ 'GET /admin/bridge': { body: status } }));
    const { list } = await openWork();
    expect(screen.getByText(note)).toBeInTheDocument();
    const add = within(list).getByRole('button', { name: 'Add' });
    expect(add).toBeDisabled();
    expect(add).toHaveAccessibleDescription(note);
    expect(calls.some((c) => c.method === 'POST')).toBe(false);
  });

  it('lets an offline Bridge take an Add, saying it waits', async () => {
    mockFetch(routes({ 'GET /admin/bridge': { body: { ...bridgeStatus, online: false } } }));
    const { list } = await openWork();
    expect(screen.getByText(/waits until it is back/)).toBeInTheDocument();
    expect(within(list).getByRole('button', { name: 'Add' })).toBeEnabled();
  });

  it('toasts an in_library refusal and shows where the recording is', async () => {
    let placed = false;
    mockFetch(
      routes({
        'POST /admin/librivox/fetch': () => {
          placed = true;
          return {
            status: 409,
            body: { error: 'already in a library', code: 'in_library', library_id: 1, path: 'x' },
          };
        },
        'GET /admin/librivox/work': () => ({
          body: work([recording({ in_library: placed ? [{ library_id: 1, path: 'x' }] : [] })]),
        }),
      }),
    );
    const { user, list } = await openWork();
    await user.click(within(list).getByRole('button', { name: 'Add' }));
    expect(await screen.findByText('That recording is already in a library.')).toBeInTheDocument();
    expect(await within(list).findByRole('link', { name: 'In library' })).toBeInTheDocument();
  });

  it("keeps naming a failed fetch's reason once the work stops naming its job", async () => {
    let asked = 0;
    const calls = mockFetch(
      routes({
        // The work names only open jobs: once the fetch failed, none.
        'GET /admin/librivox/work': () => {
          asked += 1;
          return {
            body: work([recording({ job: asked === 1 ? { id: 41, state: 'running' } : null })]),
          };
        },
        'GET /admin/bridge/jobs/41': {
          body: job({ state: 'failed', error: 'disk_full: no space left on device' }),
        },
      }),
    );
    const { list } = await openWork();
    expect(await within(list).findByText("The library's disk is full.")).toBeInTheDocument();
    await waitFor(() =>
      expect(calls.filter((c) => c.path === '/admin/librivox/work').length).toBeGreaterThan(1),
    );
    expect(within(list).getByText("The library's disk is full.")).toBeInTheDocument();
    expect(within(list).getByRole('button', { name: 'Add' })).toBeEnabled();
  });

  it('asks the work once more a moment after a fetch is done, for a book recorded just after', async () => {
    // The job reads done before the server has recorded the book: the ask at
    // the job's end still misses it, the one a moment later finds it.
    let asked = 0;
    let doneAt = 0;
    mockFetch(
      routes({
        'GET /admin/librivox/work': () => {
          asked += 1;
          const late = doneAt !== 0 && Date.now() - doneAt >= DONE_RECHECK_MS - 200;
          return {
            body: work([
              recording({
                job: asked === 1 ? { id: 41, state: 'running' } : null,
                in_library: late ? [{ library_id: 1, path: 'x' }] : [],
              }),
            ]),
          };
        },
        'GET /admin/bridge/jobs/41': () => {
          doneAt ||= Date.now();
          return { body: job({ state: 'done' }) };
        },
      }),
    );
    const { list } = await openWork();
    // Asked at the job's end: not there yet, so Add is back for now.
    await waitFor(() => expect(asked).toBeGreaterThan(1));
    expect(within(list).queryByRole('link', { name: 'In library' })).not.toBeInTheDocument();
    expect(
      await within(list).findByRole(
        'link',
        { name: 'In library' },
        { timeout: DONE_RECHECK_MS + 3000 },
      ),
    ).toBeInTheDocument();
  }, 15_000);

  it('keeps the rows when asking the work again fails, and lets go of a job that is gone', async () => {
    let asked = 0;
    mockFetch(
      routes({
        'GET /admin/librivox/work': () => {
          asked += 1;
          return asked === 1
            ? { body: work([recording({ job: { id: 41, state: 'running' } })]) }
            : asked === 2
              ? { status: 404, body: { error: 'no such work', code: 'not_found' } }
              : { body: work() };
        },
        // Gone with its library.
        'GET /admin/bridge/jobs/41': { status: 404, body: { error: 'job not found' } },
      }),
    );
    const { list } = await openWork();
    // The job is gone: the work is asked again, which fails; the row stays.
    await waitFor(() => expect(asked).toBeGreaterThan(1));
    expect(within(list).getByText('Read by Kara Shallenberg')).toBeInTheDocument();
    expect(screen.queryByText(/Couldn't load this book's recordings/)).not.toBeInTheDocument();
  });

  it("names a parked fetch's reason and holds Add while it waits", async () => {
    mockFetch(
      routes({
        'GET /admin/librivox/work': {
          body: work([recording({ job: { id: 41, state: 'needs_attention' } })]),
        },
        'GET /admin/bridge/jobs/41': {
          body: job({ state: 'needs_attention', progress: { park: { code: 'not_writable' } } }),
        },
      }),
    );
    const { list } = await openWork();
    expect(
      await within(list).findByText("The Bridge can't write to this library."),
    ).toBeInTheDocument();
    expect(within(list).getByRole('button', { name: 'Add' })).toBeDisabled();
  });
});
