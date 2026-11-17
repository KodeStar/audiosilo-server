import { useTranslation } from 'react-i18next';
import { RotateCw, Sparkles } from 'lucide-react';
import { useServerInfoLive } from '@/api/hooks';
import { Notice } from '@/components/notice';
import { Button } from '@/components/ui/button';
import { versionWatch } from '@/lib/stale-build';

// The version this page first saw, kept for the page's life: the shell remounts
// on a sign-out and back in, and the console it runs is still the one loaded.
const serverUpdated = versionWatch();

/**
 * "AudioSilo was updated" above every page once the server answers another
 * version than this tab first saw (server#197): a tab left open across an
 * upgrade would otherwise keep running the old console against the new API.
 * A page whose code is gone already reloads (route-error.tsx); this covers the
 * pages that still open.
 */
export function UpdateNotice() {
  const { t } = useTranslation();
  const server = useServerInfoLive();
  if (!serverUpdated(server.data?.version)) return null;
  return (
    <div className="mx-auto max-w-[1440px] px-4 pt-5 md:px-6 md:pt-7">
      <Notice
        tone="info"
        icon={Sparkles}
        role="status"
        title={t('routeError.staleTitle')}
        actions={
          <Button size="sm" onClick={() => window.location.reload()}>
            <RotateCw aria-hidden="true" />
            {t('routeError.reload')}
          </Button>
        }
      >
        {t('routeError.staleBody')}
      </Notice>
    </div>
  );
}
