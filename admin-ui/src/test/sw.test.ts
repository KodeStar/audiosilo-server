import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

// The server's service worker (internal/web/assets/sw.js), run against stub
// `self`/`caches`/`fetch` to check which requests go to the network first.

type FetchEvent = {
  request: { method: string; url: string; mode: string };
  respondWith: (r: Promise<unknown>) => void;
  waitUntil: (p: Promise<unknown>) => void;
};

function loadWorker(cached: Record<string, string>) {
  const handlers: Record<string, (e: FetchEvent) => void> = {};
  const self = {
    location: { origin: 'https://books.example' },
    addEventListener: (type: string, fn: (e: FetchEvent) => void) => (handlers[type] = fn),
  };
  const caches = {
    match: async (req: { url: string } | string) =>
      cached[typeof req === 'string' ? req : new URL(req.url).pathname],
    open: async () => ({ put: async () => {} }),
  };
  const network = vi.fn(async () => ({ ok: true, clone: () => ({}), body: 'network' }));
  const code = readFileSync(resolve(process.cwd(), '../internal/web/assets/sw.js'), 'utf8');
  new Function('self', 'caches', 'fetch', code)(self, caches, network);
  const request = async (path: string) => {
    let answer: Promise<unknown> | undefined;
    handlers.fetch({
      request: { method: 'GET', url: `https://books.example${path}`, mode: 'no-cors' },
      respondWith: (r) => (answer = r),
      waitUntil: () => {},
    });
    return answer;
  };
  return { network, request };
}

describe('service worker', () => {
  it("fetches the console's unhashed files from the network even when cached", async () => {
    const { network, request } = loadWorker({ '/admin/theme-init.js': 'stale' });
    await expect(request('/admin/theme-init.js')).resolves.toMatchObject({ body: 'network' });
    expect(network).toHaveBeenCalledTimes(1);
  });

  it('serves hashed console assets from the cache first', async () => {
    const { request } = loadWorker({ '/admin/assets/index-abc.js': 'cached' });
    await expect(request('/admin/assets/index-abc.js')).resolves.toBe('cached');
  });

  it('leaves the API alone', async () => {
    const { network, request } = loadWorker({});
    expect(await request('/api/v1/me')).toBeUndefined();
    expect(network).not.toHaveBeenCalled();
  });
});
