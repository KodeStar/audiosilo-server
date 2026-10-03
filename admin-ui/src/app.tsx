import { CSPProvider } from '@base-ui/react/csp-provider';
import { QueryClientProvider, type QueryClient } from '@tanstack/react-query';
import { RouterProvider } from '@tanstack/react-router';
import { Toaster } from '@/components/ui/toast';
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
}: {
  router: ReturnType<typeof createAppRouter>;
  queryClient: QueryClient;
}) {
  // The console runs under `style-src 'self'` with no nonce, so Base UI must not
  // render its inline <style> tags; the one rule it needs lives in globals.css.
  return (
    <CSPProvider disableStyleElements>
      <QueryClientProvider client={queryClient}>
        <ThemeProvider>
          <Toaster>
            <SessionProvider>
              <RouterProvider router={router} />
            </SessionProvider>
          </Toaster>
        </ThemeProvider>
      </QueryClientProvider>
    </CSPProvider>
  );
}
