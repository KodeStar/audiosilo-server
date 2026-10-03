import { Suspense, lazy } from 'react';
import { useParams } from '@tanstack/react-router';
import type { Destination, DestinationKey } from '@/components/shell/destinations';
import { ComingSoon } from '@/features/coming-soon/coming-soon';
import { NotFound } from '@/features/not-found';
import { PageSkeleton } from '@/components/page';

// Each screen is its own chunk, loaded on first visit (with its form, drag and
// drop and QR libraries), so the first paint only carries the shell and Overview.
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
const SettingsPage = lazy(() =>
  import('@/features/settings/settings-page').then((m) => ({ default: m.SettingsPage })),
);

/** The screens built so far, by destination and section. */
const PAGES: Partial<Record<DestinationKey, Record<string, React.ComponentType>>> = {
  library: { libraries: LibrariesPage },
  people: { people: PeoplePage, invites: InvitesPage, shares: SharesPage },
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
