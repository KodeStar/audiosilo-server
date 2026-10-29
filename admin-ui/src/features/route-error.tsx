import { useEffect } from 'react';
import { Link, type ErrorComponentProps } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { RotateCw, TriangleAlert } from 'lucide-react';
import { Button, buttonVariants } from '@/components/ui/button';
import { isChunkLoadError, reloadOnce } from '@/lib/stale-build';

/**
 * The router's error page (defaultErrorComponent), in place of TanStack's
 * unstyled one: a page that failed to render shows this inside the shell, the
 * nav still there. A page whose chunk is gone is a console updated since the
 * tab opened: it reloads into the new build once (stale-build.ts) and, past
 * that, says so and offers Reload.
 */
export function RouteError({ error }: ErrorComponentProps) {
  const { t } = useTranslation();
  const stale = isChunkLoadError(error);
  useEffect(() => {
    if (stale) reloadOnce();
  }, [stale]);
  return (
    <div
      role="alert"
      className="mx-auto flex max-w-[1440px] flex-col items-center gap-2.5 px-4 py-16 text-center"
    >
      <span className="grid size-10 place-items-center rounded-[11px] bg-destructive-soft text-destructive">
        <TriangleAlert className="size-5" aria-hidden="true" />
      </span>
      <h2 className="h2">{t(stale ? 'routeError.staleTitle' : 'routeError.title')}</h2>
      <p className="max-w-md text-muted-foreground">
        {t(stale ? 'routeError.staleBody' : 'routeError.body')}
      </p>
      <div className="mt-2 flex flex-wrap justify-center gap-2">
        <Button onClick={() => window.location.reload()}>
          <RotateCw aria-hidden="true" />
          {t('routeError.reload')}
        </Button>
        <Link to="/" className={buttonVariants({ variant: 'outline' })}>
          {t('notFound.home')}
        </Link>
      </div>
    </div>
  );
}
