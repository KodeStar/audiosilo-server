import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Clock, Copy, Link2, LoaderCircle, RefreshCw, TimerOff } from 'lucide-react';
import type { BridgePairCode, BridgeStatus } from '@/api/types';
import { CopyField } from '@/components/copy-field';
import { Notice } from '@/components/notice';
import { Button } from '@/components/ui/button';
import {
  Dialog,
  DialogBody,
  DialogClose,
  DialogContent,
  DialogFooter,
} from '@/components/ui/dialog';
import { copyWithToast } from '@/lib/clipboard';
import { formatClock } from '@/lib/format';
import { toast } from '@/lib/toast';
import { bridgeServerUrl, codeExchanged, COMPOSE_SERVER_URL, secondsLeft } from './bridge-model';

/** A code this page made, and the pairing it replaces (to tell when a Bridge takes it). */
export interface ShownCode extends BridgePairCode {
  pairedAtBefore?: string;
  /** A Bridge was paired when the code was made: it is replaced once the code is used. */
  replacing: boolean;
}

/**
 * The pairing code, shown once: large and copyable, with its expiry counting
 * down, the server address, and how to give both to the Bridge. The page's
 * status poll (every 3 s while a code waits) closes it once a Bridge takes the
 * code.
 */
export function PairDialog({
  shown,
  status,
  onClose,
  onNewCode,
  making,
}: {
  shown: ShownCode | null;
  status: BridgeStatus | undefined;
  onClose: () => void;
  onNewCode: () => void;
  making: boolean;
}) {
  const { t } = useTranslation();
  const exchanged = shown !== null && codeExchanged(shown.pairedAtBefore, status);

  useEffect(() => {
    if (!exchanged) return;
    toast.add({
      title: t('bridge.pair.connected'),
      description: t('bridge.pair.connectedBody'),
      type: 'success',
    });
    onClose();
  }, [exchanged, onClose, t]);

  return (
    <Dialog open={shown !== null && !exchanged} onOpenChange={(o) => !o && onClose()}>
      <DialogContent
        title={t('bridge.pair.title')}
        description={t('bridge.pair.description')}
        icon={Link2}
      >
        {shown ? <PairBody shown={shown} onNewCode={onNewCode} making={making} /> : null}
      </DialogContent>
    </Dialog>
  );
}

function PairBody({
  shown,
  onNewCode,
  making,
}: {
  shown: ShownCode;
  onNewCode: () => void;
  making: boolean;
}) {
  const { t } = useTranslation();
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(id);
  }, []);
  const left = secondsLeft(shown.expires_at, now);
  // One address, the compose snippet's when the server's own is loopback.
  const server = bridgeServerUrl(shown.server_url);
  const env = `AUDIOSILO_BRIDGE_SERVER_URL=${server.url}\nAUDIOSILO_BRIDGE_PAIR_CODE=${shown.code}`;

  return (
    <>
      <DialogBody className="flex flex-col gap-5">
        {left > 0 ? (
          <div className="flex flex-col items-center gap-2 rounded-xl border bg-muted/50 px-4 py-5 text-center">
            <span className="text-[12px] font-semibold tracking-[0.06em] text-muted-foreground uppercase">
              {t('bridge.pair.code')}
            </span>
            <span className="font-mono text-[22px] font-semibold tracking-[0.06em] [overflow-wrap:anywhere] md:text-[26px]">
              {shown.code}
            </span>
            <span className="inline-flex items-center gap-1.5 text-[12.5px] text-muted-foreground tabular-nums">
              <Clock className="size-[15px]" aria-hidden="true" />
              {t('bridge.pair.expiresIn', { time: formatClock(left) })}
            </span>
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={() => void copyWithToast(shown.code, t('bridge.pair.code'))}
            >
              <Copy aria-hidden="true" />
              {t('bridge.pair.copyCode')}
            </Button>
          </div>
        ) : (
          <Notice
            tone="warn"
            icon={TimerOff}
            title={t('bridge.pair.expired')}
            actions={
              <Button size="sm" disabled={making} onClick={onNewCode}>
                {making ? (
                  <LoaderCircle className="animate-spin" aria-hidden="true" />
                ) : (
                  <RefreshCw aria-hidden="true" />
                )}
                {t('bridge.pair.newCode')}
              </Button>
            }
          >
            {t('bridge.pair.expiredBody')}
          </Notice>
        )}

        <div className="flex flex-col gap-1.5">
          <CopyField id="bridge-server-url" label={t('bridge.pair.serverUrl')} value={server.url} />
          <p className="text-[12.5px] text-muted-foreground">
            {server.loopback
              ? t('bridge.pair.addressLoopback', {
                  compose: COMPOSE_SERVER_URL,
                  loopback: shown.server_url,
                })
              : t('bridge.pair.addressHint', { compose: COMPOSE_SERVER_URL })}
          </p>
        </div>

        <ol className="ml-5 flex list-decimal flex-col gap-1.5 text-[13.5px]">
          <li>{t('bridge.pair.step.start')}</li>
          <li>{t('bridge.pair.step.paste')}</li>
        </ol>
        <div className="flex flex-col gap-1.5">
          <span className="text-[13px] text-muted-foreground">{t('bridge.pair.envIntro')}</span>
          <pre className="overflow-x-auto rounded-lg bg-muted px-3 py-2.5 font-mono text-[12px] leading-5">
            {env}
          </pre>
          <div>
            <Button
              type="button"
              variant="ghost"
              size="sm"
              disabled={left === 0}
              onClick={() => void copyWithToast(env, t('bridge.pair.envWhat'))}
            >
              <Copy aria-hidden="true" />
              {t('bridge.pair.copyEnv')}
            </Button>
          </div>
        </div>
        {shown.replacing ? (
          <p className="text-[12.5px] text-muted-foreground">{t('bridge.pair.replacing')}</p>
        ) : null}
      </DialogBody>
      <DialogFooter className="justify-between">
        <span
          className="inline-flex items-center gap-2 text-[12.5px] text-muted-foreground"
          role="status"
        >
          {left > 0 ? (
            <>
              <LoaderCircle
                className="size-4 animate-spin motion-reduce:animate-none"
                aria-hidden="true"
              />
              {t('bridge.pair.waiting')}
            </>
          ) : null}
        </span>
        <DialogClose render={<Button type="button" variant="ghost" />}>
          {t('common.done')}
        </DialogClose>
      </DialogFooter>
    </>
  );
}
