import {
  createRootRoute,
  createRoute,
  createRouter,
  lazyRouteComponent,
} from '@tanstack/react-router';
import { PageSkeleton } from '@/components/page';
import { DESTINATIONS } from '@/components/shell/destinations';
import { validateLibrarySearch, type LibrarySearch } from '@/features/library/library-search';
import { NotFound } from '@/features/not-found';
import { OverviewPage } from '@/features/overview/overview-page';
import { USER_TABS, type UserTab } from '@/features/people/people-model';
import { SectionPage } from '@/features/section-page';
import { ISSUE_KINDS, type IssueKind } from '@/api/types';
import { parseBookSearch, type BookSearch } from '@/lib/book-route';
import { Root } from '@/root';

// Code-based TanStack Router routes under the /admin basepath. TanStack Router
// over React Router: typed params and search params (list filters, tabs and the
// selected item live in the URL, per the style guide), and it shares conventions
// with TanStack Query/Table. Code-based routes avoid the file-route codegen step.

const rootRoute = createRootRoute({ component: Root, notFoundComponent: NotFound });

const homeRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/',
  component: OverviewPage,
});

/** Search params a destination's sections read. */
export interface SectionSearch extends LibrarySearch {
  /** Libraries: open the Add library dialog (the first-run call to action). */
  add?: true;
  /** People: open the invite dialog (the palette's "Invite someone"). */
  invite?: true;
  /** Shares: the selected share. */
  share?: number;
  /** Health > Issues: the open category, and whether it lists the ignored books. */
  issue?: IssueKind;
  ignored?: true;
}

function validateSectionSearch(s: Record<string, unknown>): SectionSearch {
  const out: SectionSearch = validateLibrarySearch(s);
  const flag = (v: unknown) => v === true || v === 1 || v === '1';
  if (flag(s.add)) out.add = true;
  if (flag(s.invite)) out.invite = true;
  const share = Number(s.share);
  if (Number.isInteger(share) && share > 0) out.share = share;
  if (ISSUE_KINDS.includes(s.issue as IssueKind)) out.issue = s.issue as IssueKind;
  if (flag(s.ignored)) out.ignored = true;
  return out;
}

// One route per destination; SectionPage picks the section's screen, or its
// "coming in this redesign" placeholder.
const destinationRoutes = DESTINATIONS.map((d) =>
  createRoute({
    getParentRoute: () => rootRoute,
    path: d.route,
    validateSearch: validateSectionSearch,
    component: () => <SectionPage destination={d} />,
  }),
);

const userRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/people/user/$userId',
  validateSearch: (s: Record<string, unknown>): { tab?: UserTab } =>
    USER_TABS.includes(s.tab as UserTab) && s.tab !== USER_TABS[0] ? { tab: s.tab as UserTab } : {},
  component: lazyRouteComponent(() => import('@/features/people/user-page'), 'UserPage'),
  pendingComponent: PageSkeleton,
});

// A book's page, addressed by its identity (?library=&path=), never an internal id.
// Search params that don't name a book render the page's not-found state.
const bookRouteDef = createRoute({
  getParentRoute: () => rootRoute,
  path: '/library/book',
  validateSearch: (s: Record<string, unknown>): BookSearch =>
    parseBookSearch(s) ?? { library: 0, path: '' },
  component: lazyRouteComponent(() => import('@/features/book/book-page'), 'BookPage'),
  pendingComponent: PageSkeleton,
});

const routeTree = rootRoute.addChildren([homeRoute, userRoute, bookRouteDef, ...destinationRoutes]);

export function createAppRouter(
  opts: { history?: Parameters<typeof createRouter>[0]['history'] } = {},
) {
  return createRouter({ routeTree, basepath: '/admin', defaultPreload: 'intent', ...opts });
}

declare module '@tanstack/react-router' {
  interface Register {
    router: ReturnType<typeof createAppRouter>;
  }
}
