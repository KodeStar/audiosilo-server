import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { setToken } from '@/api/token';
import type { BridgeEstimate } from '@/api/types';
import { toast } from '@/lib/toast';
import { mockFetch } from '@/test/fetch-mock';
import { adminBook } from '@/test/library-fixtures';
import { SeriesGenerateDialog } from './series-generate-dialog';

// The series batch: a book whose chapters started being found after the check
// (the server's 409 detect_open) is passed over and named, not the end of the
// batch.

const books = [1, 2, 3].map((n) =>
  adminBook({
    path: `Brandon Sanderson/Mistborn/0${n}`,
    title: ['The Final Empire', 'The Well of Ascension', 'The Hero of Ages'][n - 1],
    series: 'Mistborn',
    series_index: n,
  }),
);

function estimate(path: string): BridgeEstimate {
  return {
    kind: 'sidecars.generate',
    library_id: 1,
    path,
    source: 'audio',
    duration_seconds: 3600,
    asr_hours: 0.25,
    agent_cost_usd: 1.6,
    subscription: false,
    agent_ready: true,
    can_contribute: false,
    figures: true,
    writable: true,
    can_describe: false,
    has: {
      community: { characters: false, recaps: false, description: false },
      local: { characters: false, recaps: false, description: false },
    },
    earlier: [],
  };
}

beforeEach(() => setToken('stored'));
afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe('generate for a series', () => {
  it('passes over a book whose chapters are being found, naming it, and queues the rest', async () => {
    const calls = mockFetch({
      'GET /admin/bridge/estimate': (req) => ({ body: estimate(req.query.get('path') ?? '') }),
      'POST /admin/bridge/jobs': (req) =>
        (req.body as { path: string }).path === books[1].path
          ? {
              status: 409,
              body: { error: 'a chapter detection job is open', code: 'detect_open' },
            }
          : { status: 201, body: { id: 9, state: 'queued', cancel_requested: false } },
    });
    const added = vi.spyOn(toast, 'add');
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const user = userEvent.setup();
    render(
      <QueryClientProvider client={qc}>
        <SeriesGenerateDialog open onOpenChange={() => {}} series="Mistborn" books={books} />
      </QueryClientProvider>,
    );
    const dialog = await screen.findByRole('dialog');
    await user.click(await within(dialog).findByRole('button', { name: 'Queue 3 books' }));
    await waitFor(() => expect(added).toHaveBeenCalled());
    // Every book was asked for: the refusal didn't stop the batch.
    expect(
      calls
        .filter((c) => c.method === 'POST' && c.path === '/admin/bridge/jobs')
        .map((c) => (c.body as { path: string }).path),
    ).toEqual(books.map((b) => b.path));
    expect(added.mock.calls[0][0]).toMatchObject({
      title: '2 books queued',
      description:
        'Chapters still being found (generate once that has finished: it reuses the transcription): The Well of Ascension.',
    });
  });
});
