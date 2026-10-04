import { Suspense, lazy } from 'react';
import { useParams } from '@tanstack/react-router';
import type { Destination, DestinationKey } from '@/components/shell/destinations';
import { NotFound } from '@/features/not-found';
import { PageSkeleton } from '@/components/page';

// Each screen is its own chunk, loaded on first visit (with its form, drag and
// drop and QR libraries), so the first paint only carries the shell and Overview.
const BooksPage = lazy(() =>
  import('@/features/library/books/books-page').then((m) => ({ default: m.BooksPage })),
);
const PeopleScreen = lazy(() =>
  import('@/features/library/people/people-screen').then((m) => ({ default: m.PeopleScreen })),
);
/** Library > Authors: everyone credited as an author, most books first. */
const AuthorsPage = () => <PeopleScreen field="author" />;
/** Library > Narrators: everyone credited as a narrator, most hours first. */
const NarratorsPage = () => <PeopleScreen field="narrator" />;
const SeriesPage = lazy(() =>
  import('@/features/library/series/series-page').then((m) => ({ default: m.SeriesPage })),
);
const FoldersPage = lazy(() =>
  import('@/features/library/folders/folders-page').then((m) => ({ default: m.FoldersPage })),
);
const LibrariesPage = lazy(() =>
  import('@/features/libraries/libraries-page').then((m) => ({ default: m.LibrariesPage })),
);
const PeoplePage = lazy(() =>
  import('@/features/people/people-page').then((m) => ({ default: m.PeoplePage })),
);
const InvitesPage = lazy(() =>
  import('@/features/people/invites-page').then((m) => ({ default: m.InvitesPage })),
);
const SharesPage = lazy(() =>
  import('@/features/shares/shares-page').then((m) => ({ default: m.SharesPage })),
);
const IssuesPage = lazy(() =>
  import('@/features/health/issues-page').then((m) => ({ default: m.IssuesPage })),
);
const JobsPage = lazy(() =>
  import('@/features/health/jobs-page').then((m) => ({ default: m.JobsPage })),
);
const DevicesPage = lazy(() =>
  import('@/features/people/devices-page').then((m) => ({ default: m.DevicesPage })),
);
const ActivityPage = lazy(() =>
  import('@/features/activity/activity-page').then((m) => ({ default: m.ActivityPage })),
);
const LivePage = lazy(() =>
  import('@/features/activity/live-page').then((m) => ({ default: m.LivePage })),
);
const SessionsPage = lazy(() =>
  import('@/features/activity/sessions-page').then((m) => ({ default: m.SessionsPage })),
);
const YearPage = lazy(() =>
  import('@/features/activity/year-page').then((m) => ({ default: m.YearPage })),
);
const SystemPage = lazy(() =>
  import('@/features/health/system-page').then((m) => ({ default: m.SystemPage })),
);
const LogsPage = lazy(() =>
  import('@/features/logs/logs-page').then((m) => ({ default: m.LogsPage })),
);
const AuditPage = lazy(() =>
  import('@/features/audit/audit-page').then((m) => ({ default: m.AuditPage })),
);
const AboutPage = lazy(() =>
  import('@/features/about/about-page').then((m) => ({ default: m.AboutPage })),
);
const SettingsPage = lazy(() =>
  import('@/features/settings/settings-page').then((m) => ({ default: m.SettingsPage })),
);

/** Every section's screen, by destination. */
// eslint-disable-next-line react-refresh/only-export-components -- exported for the test that every section has a screen
export const PAGES: Record<DestinationKey, Record<string, React.ComponentType>> = {
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
  server: { settings: SettingsPage, logs: LogsPage, audit: AuditPage, about: AboutPage },
};

/** A destination's routed section: its screen, or a 404. */
export function SectionPage({ destination }: { destination: Destination }) {
  const { section } = useParams({ strict: false }) as { section?: string };
  const active = section ?? destination.sections[0];
  if (!destination.sections.includes(active)) return <NotFound />;
  const Page = PAGES[destination.key]?.[active];
  if (!Page) return <NotFound />;
  return (
    <Suspense fallback={<PageSkeleton />}>
      <Page />
    </Suspense>
  );
}
