import type { BookRef } from '@/api/types';
import type { MockRoute } from './fetch-mock';
import {
  admin,
  issuesSummary,
  libraries,
  serverInfo,
  settings,
  stats,
  updateStatus,
} from './fixtures';

/**
 * The calls every signed-in screen makes (session, server info, the shell's
 * library health line, the overview), plus `over` for the screen under test.
 */
export function signedInRoutes(over: Record<string, MockRoute> = {}): Record<string, MockRoute> {
  return {
    'GET /server': { body: serverInfo },
    'GET /me': { body: admin },
    'GET /admin/stats': { body: stats() },
    'GET /admin/settings': { body: settings },
    // The Overview's server card: up to date.
    'GET /admin/update': { body: updateStatus() },
    'GET /admin/libraries': { body: { libraries: libraries() } },
    // The overview's "needs attention" card.
    'GET /admin/issues': { body: issuesSummary() },
    // Who is listening (Overview, People) and everyone's devices (People): nobody, by default.
    'GET /admin/sessions/live': { body: { sessions: [] } },
    'GET /admin/devices': { body: { devices: [] } },
    // What the palette searches while typing: nothing, unless a test says so.
    'GET /admin/books': { body: { books: [] } },
    'GET /admin/authors': { body: { authors: [], merge_suggestions: [], unknown: 0 } },
    'GET /admin/narrators': { body: { narrators: [], merge_suggestions: [], unknown: 0 } },
    'GET /admin/series': { body: { series: [] } },
    'GET /admin/users': { body: { users: [] } },
    'GET /admin/shares': { body: { shares: [] } },
    // No cover art by default: every book gets its generated cover.
    'POST /admin/covers': (req) => ({
      body: {
        covers: (req.body as { books: BookRef[] }).books.map((b) => ({ ...b, data: '' })),
      },
    }),
    ...over,
  };
}
