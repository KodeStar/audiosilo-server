import { CSPProvider } from '@base-ui/react/csp-provider';
import { QueryClientProvider, type QueryClient } from '@tanstack/react-query';
import { RouterProvider } from '@tanstack/react-router';
import { Toaster } from '@/components/ui/toast';
import type { OIDCReturn } from '@/lib/oidc-return';
import { SessionProvider } from '@/lib/session-provider';
import { ThemeProvider } from '@/lib/theme-provider';
import type { createAppRouter } from '@/router';

/**
 * The whole console: providers + router. main.tsx mounts it; tests render the
 * same tree with a memory history.
 */
export function App({
  router,
  queryClient,
  oidcReturn = null,
}: {
  router: ReturnType<typeof createAppRouter>;
  queryClient: QueryClient;
  /** The single sign-on's return the console was opened by (main.tsx reads it from the address). */
  oidcReturn?: OIDCReturn | null;
}) {
  // The console runs under `style-src 'self'` with no nonce, so Base UI must not
  // render its inline <style> tags; the one rule it needs lives in globals.css.
  return (
    <CSPProvider disableStyleElements>
      <QueryClientProvider client={queryClient}>
        <ThemeProvider>
          <Toaster>
            <SessionProvider oidcReturn={oidcReturn}>
              <RouterProvider router={router} />
            </SessionProvider>
          </Toaster>
        </ThemeProvider>
      </QueryClientProvider>
    </CSPProvider>
  );
}
