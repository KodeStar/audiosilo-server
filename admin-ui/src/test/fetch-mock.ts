import { vi } from 'vitest';
import { serverInfo } from './fixtures';

export interface MockRequest {
  method: string;
  /** API path without the /api/v1 prefix or query string, e.g. "/admin/stats". */
  path: string;
  query: URLSearchParams;
  headers: Record<string, string>;
  body: unknown;
}

export type MockReply =
  | { status?: number; body?: unknown; raw?: BodyInit; headers?: Record<string, string> }
  | 'network-error';
export type MockRoute = MockReply | ((req: MockRequest) => MockReply | Promise<MockReply>);

/**
 * Routes every test gets unless it mocks them: the sign-in page asks whether
 * /setup is open (closed: the server's redirect to /admin) and how to sign in
 * (GET /server: the fixture's methods, with no single sign-on).
 */
const DEFAULT_ROUTES: Record<string, MockRoute> = {
  'HEAD /setup': { status: 303 },
  'GET /server': { body: serverInfo },
};

/**
 * Stubs global fetch with a route table keyed "METHOD /path". Unmatched calls
 * fail the test loudly (404 + a console error) so a missing mock is obvious.
 * Returns the recorded requests.
 */
export function mockFetch(routes: Record<string, MockRoute>) {
  const calls: MockRequest[] = [];
  const fn = vi.fn(async (input: RequestInfo | URL, init: RequestInit = {}) => {
    const url = new URL(String(input), 'http://localhost');
    const req: MockRequest = {
      method: (init.method ?? 'GET').toUpperCase(),
      path: url.pathname.replace(/^\/api\/v1/, ''),
      query: url.searchParams,
      headers: (init.headers ?? {}) as Record<string, string>,
      body: typeof init.body === 'string' ? JSON.parse(init.body) : undefined,
    };
    calls.push(req);
    const key = `${req.method} ${req.path}`;
    const route = routes[key] ?? DEFAULT_ROUTES[key];
    if (!route) {
      console.error(`unmocked fetch: ${req.method} ${req.path}`);
      return new Response(JSON.stringify({ error: 'not mocked' }), { status: 404 });
    }
    // A route may answer later (a Promise), to test replies arriving out of order.
    const reply = await (typeof route === 'function' ? route(req) : route);
    if (reply === 'network-error') throw new TypeError('Failed to fetch');
    const status = reply.status ?? 200;
    if (reply.raw !== undefined) return new Response(reply.raw, { status, headers: reply.headers });
    const text = reply.body === undefined ? '' : JSON.stringify(reply.body);
    return new Response(status === 204 ? null : text, { status });
  });
  vi.stubGlobal('fetch', fn);
  return calls;
}
