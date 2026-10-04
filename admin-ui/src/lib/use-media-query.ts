import { useCallback, useMemo, useSyncExternalStore } from 'react';

/** Whether a media query matches, kept current (false where matchMedia always says no, as in jsdom). */
export function useMediaQuery(query: string): boolean {
  const mq = useMemo(() => window.matchMedia(query), [query]);
  const subscribe = useCallback(
    (onChange: () => void) => {
      mq.addEventListener('change', onChange);
      return () => mq.removeEventListener('change', onChange);
    },
    [mq],
  );
  return useSyncExternalStore(subscribe, () => mq.matches);
}
