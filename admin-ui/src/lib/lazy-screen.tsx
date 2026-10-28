import { useEffect, useState, type ComponentType } from 'react';
import { PageSkeleton } from '@/components/page';

/** A section's screen: rendered by SectionPage, its chunk fetched ahead by `preload`. */
export type Screen<P extends object = object> = ComponentType<P> & {
  preload: () => Promise<unknown>;
};

/**
 * A screen in its own chunk. `preload` fetches it (once): the console's
 * destination routes call it from their loader (router.tsx, section-page.tsx),
 * so the chunk loads beside the session check, not after it. Until the chunk
 * is in, the screen shows the page skeleton by itself rather than suspending
 * (React.lazy): React holds a Suspense boundary's content back for up to 300 ms
 * after its fallback shows (the fallback throttle), and the page's own requests
 * wait for that commit (server#245). A chunk that fails to load throws in render, to the
 * router's error boundary (no destination route sets its own), as a lazy screen's did.
 */
export function lazyScreen<P extends object = object>(
  load: () => Promise<ComponentType<P>>,
): Screen<P> {
  let loaded: ComponentType<P> | undefined;
  let loading: Promise<ComponentType<P>> | undefined;
  const preload = () =>
    (loading ??= load().then(
      (C) => (loaded = C),
      (err: unknown) => {
        loading = undefined; // a failed preload (offline) leaves the next try its own fetch
        throw err;
      },
    ));
  function Screen(props: P) {
    const [Ready, setReady] = useState(() => loaded);
    const [, setFailed] = useState<never>();
    useEffect(() => {
      if (Ready) return;
      let live = true;
      preload().then(
        (C) => live && setReady(() => C),
        (err: unknown) =>
          live &&
          setFailed(() => {
            throw err;
          }),
      );
      return () => {
        live = false;
      };
    }, [Ready]);
    return Ready ? <Ready {...props} /> : <PageSkeleton />;
  }
  return Object.assign(Screen, { preload });
}
