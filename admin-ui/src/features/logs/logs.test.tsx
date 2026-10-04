import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { setToken } from '@/api/token';
import type { LogEntry } from '@/api/types';
import { mockFetch } from '@/test/fetch-mock';
import { renderApp } from '@/test/render-app';
import { signedInRoutes } from '@/test/routes';

const entry = (seq: number, level: LogEntry['level'], message: string): LogEntry => ({
  seq,
  time: '2026-10-04T12:00:00Z',
  level,
  message,
  attrs: [{ key: 'library', value: 'Fiction' }],
});

beforeEach(() => setToken('stored'));

describe('logs', () => {
  it('shows the newest lines, filters by level, and tails new ones', async () => {
    let polls = 0;
    const calls = mockFetch(
      signedInRoutes({
        'GET /admin/logs': (req) => {
          if (req.query.get('after')) {
            polls++;
            return {
              body: {
                entries: polls === 1 ? [entry(3, 'error', 'ffmpeg failed')] : [],
                last_seq: 3,
                truncated: false,
              },
            };
          }
          const warn = req.query.get('level') === 'warn';
          return {
            body: {
              entries: warn
                ? [entry(2, 'warn', 'root unavailable')]
                : [entry(1, 'info', 'scan started'), entry(2, 'warn', 'root unavailable')],
              last_seq: 2,
              truncated: false,
            },
          };
        },
      }),
    );
    renderApp('/server/logs');
    const log = await screen.findByRole('log', { name: 'Server log' });
    expect(await screen.findByText('scan started')).toBeInTheDocument();
    expect(log).toHaveTextContent('library=Fiction');
    // The live tail asks only for lines after the last one it has.
    expect(await screen.findByText('ffmpeg failed', {}, { timeout: 4000 })).toBeInTheDocument();
    expect(calls.find((c) => c.query.get('after'))?.query.get('after')).toBe('2');

    await userEvent.setup().click(screen.getByRole('button', { name: 'Warnings' }));
    await waitFor(() => expect(screen.queryByText('scan started')).not.toBeInTheDocument());
    expect(screen.getByText('root unavailable')).toBeInTheDocument();
    expect(calls.some((c) => c.query.get('level') === 'warn')).toBe(true);
  });

  it('says when nothing matches', async () => {
    mockFetch(
      signedInRoutes({
        'GET /admin/logs': { body: { entries: [], last_seq: 0, truncated: false } },
      }),
    );
    renderApp('/server/logs');
    expect(await screen.findByText('Nothing logged yet.')).toBeInTheDocument();
  });
});
