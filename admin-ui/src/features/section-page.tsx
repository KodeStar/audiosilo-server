import { useParams } from '@tanstack/react-router';
import type { Destination, DestinationKey } from '@/components/shell/destinations';
import { NotFound } from '@/features/not-found';
import { lazyScreen, type Screen } from '@/lib/lazy-screen';

// Each screen is its own chunk, loaded on first visit (with its form, drag and
// drop and QR libraries), so the first paint only carries the shell and Overview.
// The destination routes fetch it from their loader (preloadSection), beside the
// session check (lib/lazy-screen.tsx).

const BooksPage = lazyScreen(() =>
  import('@/features/library/books/books-page').then((m) => m.BooksPage),
);
const loadPeople = () => import('@/features/library/people/people-screen');
/** Library > Authors: everyone credited as an author, most books first. */
const AuthorsPage = lazyScreen(() =>
  loadPeople().then(({ PeopleScreen }) => () => <PeopleScreen field="author" />),
);
/** Library > Narrators: everyone credited as a narrator, most hours first. */
const NarratorsPage = lazyScreen(() =>
  loadPeople().then(({ PeopleScreen }) => () => <PeopleScreen field="narrator" />),
);
const SeriesPage = lazyScreen(() =>
  import('@/features/library/series/series-page').then((m) => m.SeriesPage),
);
const FoldersPage = lazyScreen(() =>
  import('@/features/library/folders/folders-page').then((m) => m.FoldersPage),
);
const LibrariesPage = lazyScreen(() =>
  import('@/features/libraries/libraries-page').then((m) => m.LibrariesPage),
);
const PeoplePage = lazyScreen(() =>
  import('@/features/people/people-page').then((m) => m.PeoplePage),
);
const InvitesPage = lazyScreen(() =>
  import('@/features/people/invites-page').then((m) => m.InvitesPage),
);
const SharesPage = lazyScreen(() =>
  import('@/features/shares/shares-page').then((m) => m.SharesPage),
);
const IssuesPage = lazyScreen(() =>
  import('@/features/health/issues-page').then((m) => m.IssuesPage),
);
const JobsPage = lazyScreen(() => import('@/features/health/jobs-page').then((m) => m.JobsPage));
const DevicesPage = lazyScreen(() =>
  import('@/features/people/devices-page').then((m) => m.DevicesPage),
);
const ActivityPage = lazyScreen(() =>
  import('@/features/activity/activity-page').then((m) => m.ActivityPage),
);
const LivePage = lazyScreen(() => import('@/features/activity/live-page').then((m) => m.LivePage));
const SessionsPage = lazyScreen(() =>
  import('@/features/activity/sessions-page').then((m) => m.SessionsPage),
);
const YearPage = lazyScreen(() => import('@/features/activity/year-page').then((m) => m.YearPage));
const SystemPage = lazyScreen(() =>
  import('@/features/health/system-page').then((m) => m.SystemPage),
);
const LogsPage = lazyScreen(() => import('@/features/logs/logs-page').then((m) => m.LogsPage));
const EventsPage = lazyScreen(() =>
  import('@/features/events/events-page').then((m) => m.EventsPage),
);
const AuditPage = lazyScreen(() => import('@/features/audit/audit-page').then((m) => m.AuditPage));
const AboutPage = lazyScreen(() => import('@/features/about/about-page').then((m) => m.AboutPage));
const SettingsPage = lazyScreen(() =>
  import('@/features/settings/settings-page').then((m) => m.SettingsPage),
);

/** Every section's screen, by destination. */
// eslint-disable-next-line react-refresh/only-export-components -- exported for the test that every section has a screen
export const PAGES: Record<DestinationKey, Record<string, Screen>> = {
  library: {
    books: BooksPage,
    authors: AuthorsPage,
    series: SeriesPage,
    narrators: NarratorsPage,
    folders: FoldersPage,
    libraries: LibrariesPage,
  },
  people: { people: PeoplePage, invites: InvitesPage, shares: SharesPage, devices: DevicesPage },
  activity: { overview: ActivityPage, live: LivePage, sessions: SessionsPage, year: YearPage },
  health: { issues: IssuesPage, jobs: JobsPage, system: SystemPage },
  server: {
    settings: SettingsPage,
    events: EventsPage,
    logs: LogsPage,
    audit: AuditPage,
    about: AboutPage,
  },
};

/** A section's screen (the destination's first without one), if it has one. */
function sectionScreen(destination: Destination, section: string | undefined): Screen | undefined {
  const pages = PAGES[destination.key];
  const key = section ?? destination.sections[0];
  // Own keys only: a URL naming an inherited property (/library/constructor) is a 404.
  return Object.hasOwn(pages, key) ? pages[key] : undefined;
}

/**
 * Starts fetching a section's chunk (a destination route's loader, so also on a
 * link's hover); a failed fetch is left to SectionPage, which fetches again.
 */
// eslint-disable-next-line react-refresh/only-export-components -- the routes' loader
export function preloadSection(destination: Destination, section: string | undefined) {
  void sectionScreen(destination, section)
    ?.preload()
    .catch(() => {});
}

/** A destination's routed section: its screen, or a 404. */
export function SectionPage({ destination }: { destination: Destination }) {
  const { section } = useParams({ strict: false }) as { section?: string };
  // Every listed section has a screen (section-page.test.ts), so a missing one is a 404.
  const Page = sectionScreen(destination, section);
  if (!Page) return <NotFound />;
  return <Page />;
}
