import { Suspense, lazy } from 'react';
import { useParams } from '@tanstack/react-router';
import type { Destination, DestinationKey } from '@/components/shell/destinations';
import { ComingSoon } from '@/features/coming-soon/coming-soon';
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
const SettingsPage = lazy(() =>
  import('@/features/settings/settings-page').then((m) => ({ default: m.SettingsPage })),
);

/** The screens built so far, by destination and section. */
const PAGES: Partial<Record<DestinationKey, Record<string, React.ComponentType>>> = {
  library: {
    books: BooksPage,
    authors: AuthorsPage,
    series: SeriesPage,
    narrators: NarratorsPage,
    folders: FoldersPage,
    libraries: LibrariesPage,
  },
  people: { people: PeoplePage, invites: InvitesPage, shares: SharesPage },
  health: { issues: IssuesPage, jobs: JobsPage },
  server: { settings: SettingsPage },
};

/** A destination's routed section: its screen, its placeholder, or a 404. */
export function SectionPage({ destination }: { destination: Destination }) {
  const { section } = useParams({ strict: false }) as { section?: string };
  const active = section ?? destination.sections[0];
  if (!destination.sections.includes(active)) return <NotFound />;
  const Page = PAGES[destination.key]?.[active];
  if (Page) {
    return (
      <Suspense fallback={<PageSkeleton />}>
        <Page />
      </Suspense>
    );
  }
  return <ComingSoon destination={destination} section={active} />;
}
