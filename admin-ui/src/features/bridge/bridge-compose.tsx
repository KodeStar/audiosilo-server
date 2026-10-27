import { Trans, useTranslation } from 'react-i18next';
import { Copy } from 'lucide-react';
import { Button } from '@/components/ui/button';
import { copyWithToast } from '@/lib/clipboard';
import { bridgeCompose, RUNNING_A_BRIDGE_DOCS_URL } from './bridge-model';
import { ExternalTextLink } from './generate-parts';

/**
 * Where to get a Bridge: the compose snippet for the server's compose file (one
 * volume per library, at the server's own paths), Copy, and the User Guide's
 * Running a Bridge for the agent container and the rest. The first-run flow's
 * Bridge step and Settings > Bridge (no Bridge paired) both show it.
 */
export function BridgeCompose({ roots }: { roots: readonly string[] }) {
  const { t } = useTranslation();
  const compose = bridgeCompose(roots);
  return (
    <div className="flex flex-col gap-2">
      <span className="text-[13px] text-muted-foreground">{t('setup.bridge.composeIntro')}</span>
      <pre className="max-h-[320px] overflow-auto rounded-lg bg-muted px-3 py-2.5 font-mono text-[12px] leading-5">
        {compose}
      </pre>
      <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
        <Button
          type="button"
          variant="ghost"
          size="sm"
          onClick={() => void copyWithToast(compose, t('setup.bridge.composeWhat'))}
        >
          <Copy aria-hidden="true" />
          {t('setup.bridge.copy')}
        </Button>
        <span className="text-[12.5px] text-muted-foreground">
          <Trans
            i18nKey="setup.bridge.agent"
            components={{
              docs: <ExternalTextLink href={RUNNING_A_BRIDGE_DOCS_URL}>{null}</ExternalTextLink>,
            }}
          />
        </span>
      </div>
    </div>
  );
}
