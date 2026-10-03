import {
  ApiError,
  api,
  fetchCover,
  setForbiddenHandler,
  setUnauthorizedHandler,
  toDataUrl,
} from './client';
import { getToken, setToken } from './token';
import { mockFetch } from '@/test/fetch-mock';
import { admin, stats } from '@/test/fixtures';

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

  it('fetches covers with the session header, never a token in the URL', async () => {
    setToken('admin-secret');
    const fetchFn = vi.fn(
      async () =>
        new Response(new Uint8Array([0xff, 0xd8, 0xff]), {
          status: 200,
          headers: { 'Content-Type': 'image/jpeg' },
        }),
    );
    vi.stubGlobal('fetch', fetchFn);
    const url = await fetchCover(3, 'Andy Weir/Project Hail Mary');
    expect(url).toBe('data:image/jpeg;base64,/9j/');
    const [target, init] = fetchFn.mock.calls[0] as unknown as [string, RequestInit];
    const u = new URL(target, 'http://x');
    expect(u.pathname).toBe('/api/v1/libraries/3/cover');
    expect(u.searchParams.get('path')).toBe('Andy Weir/Project Hail Mary');
    expect(u.searchParams.has('token')).toBe(false);
    expect(target).not.toContain('admin-secret');
    expect((init.headers as Record<string, string>).Authorization).toBe('Bearer admin-secret');
  });

  it('reports a book without art as null', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => new Response('', { status: 404 })),
    );
    await expect(fetchCover(1, 'x')).resolves.toBeNull();
  });

  it('encodes large covers without blowing the argument limit', () => {
    const big = new Uint8Array(200_000).fill(65); // "A" * 200k
    const url = toDataUrl(big, 'image/png');
    expect(url.startsWith('data:image/png;base64,QUFB')).toBe(true);
    expect(atob(url.split(',')[1])).toHaveLength(200_000);
  });
});
