import { useEffect } from 'react';
import { Link, type ErrorComponentProps } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { RotateCw, TriangleAlert } from 'lucide-react';
import { PageMessage } from '@/components/page-message';
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
    <PageMessage
      role="alert"
      icon={TriangleAlert}
      tone="bad"
      title={t(stale ? 'routeError.staleTitle' : 'routeError.title')}
      body={t(stale ? 'routeError.staleBody' : 'routeError.body')}
      actions={
        <>
          <Button onClick={() => window.location.reload()}>
            <RotateCw aria-hidden="true" />
            {t('routeError.reload')}
          </Button>
          <Link to="/" className={buttonVariants({ variant: 'outline' })}>
            {t('notFound.home')}
          </Link>
        </>
      }
    />
  );
}
