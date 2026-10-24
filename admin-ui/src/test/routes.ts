import type { AdminSettings, BookRef } from '@/api/types';
import type { MockRoute } from './fetch-mock';
import {
  admin,
  issuesSummary,
  libraries,
  podcastLibrary,
  serverInfo,
  settings,
  setupEnvelope,
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
    // The Overview's support card: not due, by default.
    'GET /admin/support': { body: { show: false } },
    'GET /admin/libraries': { body: { libraries: libraries() } },
    // The first-run flow: finished, as on any server that's set up.
    'GET /admin/setup': { body: setupEnvelope() },
    // The overview's "needs attention" card.
    'GET /admin/issues': { body: issuesSummary() },
    // Who is listening (Overview, People) and everyone's devices (People): nobody, by default.
    'GET /admin/sessions/live': { body: { sessions: [] } },
    'GET /admin/devices': { body: { devices: [] } },
    // The bell's feed: nothing has happened, by default.
    'GET /admin/events': { body: { events: [], next_before: 0 } },
    // What the palette searches while typing: nothing, unless a test says so.
    'GET /admin/books': { body: { books: [] } },
    'GET /admin/authors': { body: { authors: [], merge_suggestions: [], unknown: 0 } },
    'GET /admin/narrators': { body: { narrators: [], merge_suggestions: [], unknown: 0 } },
    'GET /admin/series': { body: { series: [] } },
    'GET /admin/users': { body: { users: [] } },
    'GET /admin/shares': { body: { shares: [] } },
    // A person's imported listening (their Listening tab): none, by default.
    'GET /admin/imports': { body: { imports: [] } },
    // The book page's Characters and recaps card: no community match, nothing generated.
    'GET /libraries/1/meta': { body: { matched: false } },
    // No cover art by default: every book gets its generated cover.
    'POST /admin/covers': (req) => ({
      body: {
        covers: (req.body as { books: BookRef[] }).books.map((b) => ({ ...b, data: '' })),
      },
    }),
    ...over,
  };
}

/** A PATCH that answers with the settings merged with what it sent. */
export function patchEcho(base: AdminSettings, extra: Partial<AdminSettings> = {}): MockRoute {
  return (req) => {
    const body = req.body as Record<string, Record<string, unknown>>;
    const next = { ...base, ...extra } as unknown as Record<string, unknown>;
    for (const [section, fields] of Object.entries(body)) {
      next[section] = { ...(next[section] as object), ...fields };
    }
    return { body: next };
  };
}

/** The libraries route with a podcast library (id 3) after `libs`. */
export function withPodcastLibrary(libs = libraries()): Record<string, MockRoute> {
  return { 'GET /admin/libraries': { body: { libraries: [...libs, podcastLibrary] } } };
}
