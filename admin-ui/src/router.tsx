import { createRootRoute, createRoute, createRouter } from '@tanstack/react-router';
import { DESTINATIONS } from '@/components/shell/destinations';
import { ComingSoon } from '@/features/coming-soon/coming-soon';
import { NotFound } from '@/features/not-found';
import { OverviewPage } from '@/features/overview/overview-page';
import { Root } from '@/root';

// Code-based TanStack Router routes under the /admin basepath. TanStack Router
// over React Router: typed params and search params (later phases keep list
// filters and tabs in the URL, per the style guide), and it shares conventions
// with TanStack Query/Table. Code-based routes avoid the file-route codegen step.

const rootRoute = createRootRoute({ component: Root, notFoundComponent: NotFound });

const homeRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/',
  component: OverviewPage,
});

// One placeholder route per destination until its phase builds real screens;
// later phases replace a destination's entry with its own routes.
const destinationRoutes = DESTINATIONS.map((d) =>
  createRoute({
    getParentRoute: () => rootRoute,
    path: d.route,
    component: () => <ComingSoon destination={d} />,
  }),
);

const routeTree = rootRoute.addChildren([homeRoute, ...destinationRoutes]);

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
