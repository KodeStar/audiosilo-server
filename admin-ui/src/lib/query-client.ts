import { QueryClient } from '@tanstack/react-query';
import { ApiError } from '@/api/client';

export function createQueryClient() {
  return new QueryClient({
    defaultOptions: {
      queries: {
        // Retry network blips and 5xx, never a 4xx: it would fail the same way again.
        retry: (failures, err) => failures < 2 && !(err instanceof ApiError && err.status < 500),
        refetchOnWindowFocus: true,
      },
    },
  });
}
