import { ApiError, api, bookQuery, setForbiddenHandler, setUnauthorizedHandler } from './client';
import { getToken, setToken } from './token';
import { mockFetch } from '@/test/fetch-mock';
import { activity, admin, stats } from '@/test/fixtures';

describe('api client', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    setUnauthorizedHandler(() => {});
    setForbiddenHandler(() => {});
  });

  it('asks who is signed in after a 403 from an admin endpoint, keeping the session', async () => {
    setToken('tok-1');
    const forbidden = vi.fn();
    setForbiddenHandler(forbidden);
    mockFetch({
      'GET /admin/users': { status: 403, body: { error: 'forbidden' } },
      'GET /libraries/1/fs': { status: 403, body: { error: 'forbidden' } },
    });
    await expect(api.users()).rejects.toMatchObject({ status: 403 });
    expect(forbidden).toHaveBeenCalledTimes(1);
    expect(getToken()).toBe('tok-1');
    // A 403 outside /admin is a plain scope refusal, not a demotion.
    await expect(api.browse(1, '')).rejects.toMatchObject({ status: 403 });
    expect(forbidden).toHaveBeenCalledTimes(1);
  });

  it('treats a successful non-JSON body (a proxy page) as no data, not a failure', async () => {
    setToken('tok-1');
    mockFetch({ 'DELETE /admin/libraries/1': { raw: '<html>ok</html>' } });
    await expect(api.deleteLibrary(1)).resolves.toBeUndefined();
  });

  it('sends paths as a query parameter, never in the URL path', async () => {
    setToken('tok-1');
    const calls = mockFetch({ 'PUT /admin/libraries/1/folder-override': { body: {} } });
    await api.setFolderOverride(1, 'A & B/Book #1', 'book');
    expect(calls[0].path).toBe('/admin/libraries/1/folder-override');
    expect(calls[0].query.get('path')).toBe('A & B/Book #1');
  });

  it('sends the bearer token and decodes JSON', async () => {
    setToken('tok-1');
    const calls = mockFetch({ 'GET /admin/stats': { body: stats() } });
    const res = await api.stats();
    expect(res.total_books).toBe(3249);
    expect(calls[0].headers.Authorization).toBe('Bearer tok-1');
  });

  it('names itself to the server on every request, signed in or not', async () => {
    const calls = mockFetch({
      'POST /auth/login': { body: { token: 't', user: admin, server_id: 's' } },
      'GET /admin/stats': { body: stats() },
    });
    await api.login('chris', 'pw');
    setToken('tok-1');
    await api.stats();
    expect(calls.map((c) => c.headers['X-AudioSilo-Client'])).toEqual([
      'AudioSilo Admin (web)',
      'AudioSilo Admin (web)',
    ]);
  });

  it('logs in with the admin-web device name and no stale header', async () => {
    const calls = mockFetch({
      'POST /auth/login': { body: { token: 't', user: admin, server_id: 's' } },
    });
    await api.login('chris', 'pw');
    expect(calls[0].body).toEqual({ username: 'chris', password: 'pw', device_name: 'admin-web' });
    expect(calls[0].headers.Authorization).toBeUndefined();
  });

  it('raises the server error envelope as ApiError', async () => {
    mockFetch({ 'GET /admin/settings': { status: 400, body: { error: 'nope' } } });
    await expect(api.settings()).rejects.toEqual(new ApiError(400, 'nope'));
  });

  it('falls back to the status text for a non-JSON error body', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(
        async () =>
          new Response('<html>bad gateway</html>', { status: 502, statusText: 'Bad Gateway' }),
      ),
    );
    await expect(api.settings()).rejects.toMatchObject({ status: 502, message: 'Bad Gateway' });
  });

  it('drops the session on a 401 when a token was sent', async () => {
    setToken('expired');
    const onUnauthorized = vi.fn();
    setUnauthorizedHandler(onUnauthorized);
    mockFetch({ 'GET /admin/stats': { status: 401, body: { error: 'unauthorized' } } });
    await expect(api.stats()).rejects.toMatchObject({ status: 401 });
    expect(getToken()).toBeNull();
    expect(onUnauthorized).toHaveBeenCalledOnce();
  });

  it('drops the session on a 401 after another tab cleared the token', async () => {
    const onUnauthorized = vi.fn();
    setUnauthorizedHandler(onUnauthorized);
    const calls = mockFetch({
      'GET /admin/stats': { status: 401, body: { error: 'unauthorized' } },
    });
    await expect(api.stats()).rejects.toMatchObject({ status: 401 });
    expect(calls[0].headers.Authorization).toBeUndefined();
    expect(onUnauthorized).toHaveBeenCalledOnce();
  });

  it('signs an explicit token out without touching the stored session', async () => {
    setToken('stored');
    const onUnauthorized = vi.fn();
    setUnauthorizedHandler(onUnauthorized);
    const calls = mockFetch({ 'POST /auth/logout': { status: 401, body: { error: 'x' } } });
    await expect(api.logout('other')).rejects.toMatchObject({ status: 401 });
    expect(calls[0].headers.Authorization).toBe('Bearer other');
    expect(getToken()).toBe('stored');
    expect(onUnauthorized).not.toHaveBeenCalled();
  });

  it('does not treat a failed sign-in as an expired session', async () => {
    const onUnauthorized = vi.fn();
    setUnauthorizedHandler(onUnauthorized);
    mockFetch({ 'POST /auth/login': { status: 401, body: { error: 'invalid credentials' } } });
    await expect(api.login('a', 'b')).rejects.toMatchObject({ status: 401 });
    expect(onUnauthorized).not.toHaveBeenCalled();
  });

  it('fetches cover thumbnails with the session header, never a token in the URL', async () => {
    setToken('admin-secret');
    const calls = mockFetch({
      'POST /admin/covers': {
        body: { covers: [{ library_id: 3, path: 'Andy Weir/Project Hail Mary', data: '' }] },
      },
    });
    const { covers } = await api.coverThumbs([
      { library_id: 3, path: 'Andy Weir/Project Hail Mary' },
    ]);
    expect(covers).toHaveLength(1);
    expect(calls[0].body).toEqual({
      books: [{ library_id: 3, path: 'Andy Weir/Project Hail Mary' }],
      size: 320,
    });
    expect(calls[0].query.has('token')).toBe(false);
    expect(calls[0].headers.Authorization).toBe('Bearer admin-secret');
  });

  it('reads either people aggregate as one shape', async () => {
    const calls = mockFetch({
      'GET /admin/authors': {
        body: { authors: [{ name: 'A', books: 1, duration: 60 }], merge_suggestions: null },
      },
      'GET /admin/narrators': { body: { narrators: null, merge_suggestions: [], unknown: 2 } },
    });
    expect(await api.people('author', 2)).toEqual({
      people: [{ name: 'A', books: 1, duration: 60 }],
      merge_suggestions: [],
      unknown: 0,
    });
    expect(await api.people('narrator')).toEqual({ people: [], merge_suggestions: [], unknown: 2 });
    expect(calls.map((c) => [c.path, c.query.get('library_id')])).toEqual([
      ['/admin/authors', '2'],
      ['/admin/narrators', null],
    ]);
  });

  it('adds many share rules in one request', async () => {
    const calls = mockFetch({ 'POST /admin/shares/7/paths': { status: 204 } });
    await api.addSharePaths(7, [
      { library_id: 1, path: 'A' },
      { library_id: 2, path: 'B' },
    ]);
    expect(calls[0].body).toEqual({
      rules: [
        { library_id: 1, path: 'A' },
        { library_id: 2, path: 'B' },
      ],
    });
  });

  it('builds catalog queries: unset values dropped, repeatable values repeated', () => {
    expect(bookQuery({})).toBe('');
    expect(
      bookQuery({ q: 'way of', format: ['m4b', 'mp3'], matched: false, author: '', limit: 60 }),
    ).toBe('?q=way+of&format=m4b&format=mp3&matched=false&limit=60');
  });

  it('sends the match search with the path and only the given terms', async () => {
    const calls = mockFetch({ 'GET /admin/libraries/1/book/match': { body: { candidates: [] } } });
    await api.matchBook(1, 'A/B & C', { asin: 'B003P2WO5E' });
    expect(Object.fromEntries(calls[0].query)).toEqual({ path: 'A/B & C', asin: 'B003P2WO5E' });
  });

  it('uploads a cover as the raw image with its type, not JSON', async () => {
    mockFetch({ 'PUT /admin/libraries/1/cover': { body: { status: 'cover set' } } });
    const image = new Blob([new Uint8Array([0xff, 0xd8, 0xff])], { type: 'image/jpeg' });
    await api.setCover(1, 'A/B', image);
    const [url, init] = vi.mocked(fetch).mock.calls[0];
    expect(String(url)).toBe('/api/v1/admin/libraries/1/cover?path=A%2FB');
    expect(init?.body).toBe(image);
    expect((init?.headers as Record<string, string>)['Content-Type']).toBe('image/jpeg');
  });

  it('carries the field a refused book edit names', async () => {
    mockFetch({
      'PATCH /admin/libraries/1/book': {
        status: 400,
        body: {
          error: 'asin: must be 10 letters or digits',
          code: 'invalid_override',
          field: 'asin',
        },
      },
    });
    const err = await api.editBook(1, 'A/B', { set: { asin: 'x' } }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect(err).toMatchObject({ status: 400, code: 'invalid_override', field: 'asin' });
  });

  it('reads the Activity period from the stats envelope, and fails if the server sends none', async () => {
    const calls = mockFetch({
      'GET /admin/stats': (req) => ({
        body: req.query.get('range') === '7d' ? { activity: activity() } : stats(),
      }),
    });
    await expect(api.activity('7d')).resolves.toMatchObject({
      range: '7d',
      totals: { sessions: 12 },
    });
    expect(calls[0].query.get('range')).toBe('7d');
    // An older server ignores ?range=: say so instead of drawing an empty page.
    await expect(api.activity('2025')).rejects.toBeInstanceOf(ApiError);
  });

  it('pages sessions and edits progress by identity, with the user as a parameter', async () => {
    const calls = mockFetch({
      'GET /admin/sessions': { body: { sessions: [], next_before: null } },
      'PATCH /admin/libraries/1/progress': { body: { progress: {} } },
      'GET /admin/devices': { body: { devices: [] } },
    });
    await api.sessions({ user_id: 2, library_id: 1, path: 'A & B/C', before: 9 });
    expect(Object.fromEntries(calls[0].query)).toEqual({
      user_id: '2',
      library_id: '1',
      path: 'A & B/C',
      before: '9',
    });
    await api.editProgress(1, 'A & B/C', 2, { finished: true, started_at: null });
    expect(calls[1].path).toBe('/admin/libraries/1/progress');
    expect(Object.fromEntries(calls[1].query)).toEqual({ path: 'A & B/C', user_id: '2' });
    expect(calls[1].body).toEqual({ finished: true, started_at: null });
    await api.devices();
    expect(calls[2].query.toString()).toBe('');
  });
});
