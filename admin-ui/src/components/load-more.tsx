import { useEffect, useRef } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/button';

/**
 * The "Show more" button under a paged list, which also asks for the next page
 * by itself once it comes near the viewport, so scrolling loads more. The
 * button stays for keyboards and for a page that failed (`auto` false: no
 * retrying on its own). Each page loaded (`loaded`) watches again, so a page too
 * short to push the button out of view still brings the next one. Without
 * IntersectionObserver (jsdom) only the button loads.
 */
export function LoadMore({
  onMore,
  loading,
  loaded,
  auto = true,
}: {
  onMore: () => void;
  loading: boolean;
  /** How many pages (or rows) are loaded: a change watches again. */
  loaded: number;
  auto?: boolean;
}) {
  const { t } = useTranslation();
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const el = ref.current;
    if (!el || loading || !auto || typeof IntersectionObserver === 'undefined') return;
    const io = new IntersectionObserver(
      (entries) => {
        if (entries.some((e) => e.isIntersecting)) {
          io.disconnect();
          onMore();
        }
      },
      { rootMargin: '600px 0px' },
    );
    io.observe(el);
    return () => io.disconnect();
  }, [loading, loaded, auto, onMore]);
  return (
    <div ref={ref} className="flex justify-center">
      <Button variant="outline" onClick={onMore} disabled={loading}>
        {loading ? t('common.loading') : t('health.more')}
      </Button>
    </div>
  );
}
