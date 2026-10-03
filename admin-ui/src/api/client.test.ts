import { ApiError, api, coverUrl, setUnauthorizedHandler } from './client';
import { getToken, setToken } from './session';
import { mockFetch } from '@/test/fetch-mock';
import { admin, stats } from '@/test/fixtures';

describe('api client', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    setUnauthorizedHandler(() => {});
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

  it('does not treat a failed sign-in as an expired session', async () => {
    const onUnauthorized = vi.fn();
    setUnauthorizedHandler(onUnauthorized);
    mockFetch({ 'POST /auth/login': { status: 401, body: { error: 'invalid credentials' } } });
    await expect(api.login('a', 'b')).rejects.toMatchObject({ status: 401 });
    expect(onUnauthorized).not.toHaveBeenCalled();
  });

  it('puts the token and path in cover URLs', () => {
    setToken('tok 2');
    const u = new URL(coverUrl(3, 'Andy Weir/Project Hail Mary'), 'http://x');
    expect(u.pathname).toBe('/api/v1/libraries/3/cover');
    expect(u.searchParams.get('path')).toBe('Andy Weir/Project Hail Mary');
    expect(u.searchParams.get('token')).toBe('tok 2');
  });
});
