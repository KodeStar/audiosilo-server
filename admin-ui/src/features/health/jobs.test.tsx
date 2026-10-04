import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { setToken } from '@/api/token';
import type { ScanRun } from '@/api/types';
import { mockFetch, type MockRoute } from '@/test/fetch-mock';
import { idle, jobsState } from '@/test/fixtures';
import { renderApp } from '@/test/render-app';
import { signedInRoutes } from '@/test/routes';

// Health > Jobs: the live scan, the queue, schedules and history with logs.

const finished: ScanRun = {
  id: 7,
  library_id: 1,
  library_name: 'Fiction',
  trigger: 'schedule',
  started_by: null,
  started_at: '2026-10-04T07:00:00Z',
  finished_at: '2026-10-04T07:01:52Z',
  status: 'ok',
  books: 2400,
  added: 14,
  updated: 3,
  moved: 1,
  removed: 0,
  errors: 0,
};

function routes(over: Record<string, MockRoute> = {}) {
  return signedInRoutes({
    'GET /admin/jobs': { body: jobsState() },
    'GET /admin/scan-runs': { body: { runs: [finished] } },
    'GET /admin/scan-runs/7': {
      body: {
        ...finished,
        log: [
          { at: '2026-10-04T07:00:00Z', level: 'info', kind: 'started', path: '/mnt/tank/fiction' },
          {
            at: '2026-10-04T07:01:00Z',
            level: 'info',
            kind: 'moved',
            path: 'Dune',
            to: 'Dune/01 - Dune',
          },
          { at: '2026-10-04T07:01:52Z', level: 'info', kind: 'finished', count: 2400 },
        ],
      },
    },
    ...over,
  });
}

beforeEach(() => setToken('stored'));
afterEach(() => vi.unstubAllGlobals());

describe('jobs', () => {
  it('shows the running scan live, the queue, and stops or cancels them', async () => {
    const calls = mockFetch(
      routes({
        'GET /admin/jobs': {
          body: jobsState({
            running: {
              id: 3,
              kind: 'scan',
              library_id: 1,
              library_name: 'Fiction',
              trigger: 'manual',
              started_by: 1,
              queued_at: '2026-10-04T08:00:00Z',
              started_at: '2026-10-04T08:00:01Z',
              progress: { ...idle, running: true, total: 400, done: 100, added: 5 },
            },
            queued: [
              {
                id: 4,
                kind: 'scan',
                library_id: 2,
                library_name: 'Kids',
                trigger: 'change',
                started_by: 1,
                queued_at: '2026-10-04T08:00:02Z',
              },
            ],
          }),
        },
        'DELETE /admin/jobs/3': { status: 204 },
        'DELETE /admin/jobs/4': { status: 204 },
      }),
    );
    const user = userEvent.setup();
    renderApp('/health/jobs');
    const now = await screen.findByRole('region', { name: 'Running now' });
    expect(within(now).getByText('Scanning Fiction')).toBeInTheDocument();
    expect(within(now).getByText('100 of 400 books checked')).toBeInTheDocument();
    expect(within(now).getByRole('progressbar')).toHaveAttribute('aria-valuenow', '25');
    const queue = screen.getByRole('region', { name: 'Waiting (1)' });
    expect(within(queue).getByText('Settings changed', { exact: false })).toBeInTheDocument();

    await user.click(within(queue).getByRole('button', { name: 'Cancel the Kids scan' }));
    await waitFor(() =>
      expect(calls.some((c) => c.method === 'DELETE' && c.path === '/admin/jobs/4')).toBe(true),
    );
    await user.click(within(now).getByRole('button', { name: 'Stop' }));
    expect(await screen.findByText('Stopped the Fiction scan')).toBeInTheDocument();
    expect(calls.some((c) => c.method === 'DELETE' && c.path === '/admin/jobs/3')).toBe(true);
  });

  it('lists schedules and history, and opens a scan log', async () => {
    mockFetch(
      routes({
        'GET /admin/jobs': {
          body: jobsState({
            schedules: [
              {
                library_id: 1,
                library_name: 'Fiction',
                schedule: 'every:6h',
                next_at: '2026-10-04T13:00:00Z',
              },
            ],
          }),
        },
      }),
    );
    const user = userEvent.setup();
    renderApp('/health/jobs');
    expect(await screen.findByText('Nothing running')).toBeInTheDocument();
    const schedules = screen.getByRole('region', { name: 'Schedules' });
    expect(within(schedules).getByText('Every 6 hours')).toBeInTheDocument();
    expect(await screen.findByText('14 new · 3 changed · 1 moved')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Log' }));
    expect(
      await screen.findByText('Moved Dune to Dune/01 - Dune; progress followed'),
    ).toBeInTheDocument();
    expect(screen.getByText('Finished: 2,400 books on disk')).toBeInTheDocument();
  });

  it('runs a scan from the menu', async () => {
    const calls = mockFetch(
      routes({ 'POST /admin/libraries/2/scan': { status: 202, body: { status: 'scan started' } } }),
    );
    const user = userEvent.setup();
    renderApp('/health/jobs');
    await user.click(await screen.findByRole('button', { name: 'Run a job' }));
    await user.click(await screen.findByRole('menuitem', { name: 'Rescan Kids' }));
    await waitFor(() => expect(calls.some((c) => c.path === '/admin/libraries/2/scan')).toBe(true));
  });
});
