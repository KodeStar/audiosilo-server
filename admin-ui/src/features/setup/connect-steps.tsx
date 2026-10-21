import { useCallback, useState } from 'react';
import { Link } from '@tanstack/react-router';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import {
  CircleCheck,
  Copy,
  ExternalLink,
  Headphones,
  Link2,
  LoaderCircle,
  RefreshCw,
  Users,
} from 'lucide-react';
import { api } from '@/api/client';
import { keys, useServerInfo, useSystem } from '@/api/hooks';
import type { PairToken } from '@/api/types';
import { Notice } from '@/components/notice';
import { QrCode } from '@/components/qr-code';
import { StatusText } from '@/components/status-text';
import { Button, buttonVariants } from '@/components/ui/button';
import { PairDialog, type ShownCode } from '@/features/bridge/pair-dialog';
import { BridgeSetupList } from '@/features/bridge/setup-list';
import { useBridge } from '@/features/bridge/use-bridge';
import { playerConnectUrl } from '@/features/book/book-model';
import { copyWithToast } from '@/lib/clipboard';
import { toastError } from '@/lib/errors';
import { useCurrentUser } from '@/lib/session';
import { usePlayerTab } from '@/lib/use-player-tab';
import { bridgeCompose } from './setup-model';
import { StepFooter } from './step-parts';
import type { StepProps } from './step-actions';

/**
 * Step 5: coming from Audiobookshelf. Points to the listening import (in a new
 * tab, so the flow stays open) and says ABS metadata.json files are read.
 * Skipped by default: Continue records it done only once the import was opened.
 */
export function AbsStep({ leave, back, last }: StepProps) {
  const { t } = useTranslation();
  const [opened, setOpened] = useState(false);
  return (
    <>
      <div className="flex flex-col gap-4">
        <p>{t('setup.abs.body')}</p>
        <ul className="ml-5 flex list-disc flex-col gap-1.5 text-[13.5px] text-muted-foreground">
          <li>{t('setup.abs.import')}</li>
          <li>{t('setup.abs.metadata')}</li>
        </ul>
        <div>
          <Link
            to="/server/{-$section}"
            params={{ section: undefined }}
            search={{ topic: 'import' }}
            target="_blank"
            rel="noreferrer"
            className={buttonVariants({ variant: 'outline' })}
            onClick={() => setOpened(true)}
          >
            <ExternalLink aria-hidden="true" />
            {t('setup.abs.open')}
          </Link>
        </div>
      </div>
      <StepFooter
        back={back}
        onSkip={() => leave('skipped')}
        onContinue={() => leave(opened ? 'done' : 'skipped')}
        last={last}
      />
    </>
  );
}

/** The phone's connect link for a pairing token (the server sends it as web_url). */
function connectUrl(p: PairToken): string {
  if (p.web_url) return p.web_url;
  const q = new URLSearchParams({ token: p.pairing_token, sid: p.server_id });
  return `${window.location.origin}/web/connect?${q.toString()}`;
}

/**
 * Step 7: connect a phone. The pairing QR (a single-use token for this admin,
 * ten minutes), the web player in a new tab with its own token, whether
 * ffmpeg is there for formats a browser can't play, and where household
 * members are added.
 */
export function PhoneStep({ leave, back, last }: StepProps) {
  const { t } = useTranslation();
  const server = useServerInfo();
  const system = useSystem();
  const pair = useQuery({
    queryKey: keys.setupPair,
    queryFn: api.pairSelf,
    staleTime: Infinity,
    gcTime: 0,
    retry: false,
  });
  const ffmpeg = system.data?.tools.find((x) => x.name === 'ffmpeg');
  const webPlayer = server.data?.capabilities.web_player ?? false;

  return (
    <>
      <div className="flex flex-col gap-5">
        <div className="flex flex-wrap items-center gap-5 rounded-xl border px-4 py-4">
          {pair.data ? (
            <QrCode text={connectUrl(pair.data)} label={t('setup.phone.qr')} />
          ) : (
            <span
              className="skel size-[168px] shrink-0 rounded-[10px]"
              role="status"
              aria-label={t('common.loading')}
            />
          )}
          <div className="flex min-w-[200px] flex-1 flex-col gap-2">
            <b className="font-semibold">{t('setup.phone.scan')}</b>
            <span className="text-[13px] text-muted-foreground">{t('setup.phone.scanBody')}</span>
            {pair.isError ? (
              <span className="text-[13px] font-medium text-destructive" role="alert">
                {t('setup.phone.failed')}
              </span>
            ) : null}
            <div>
              <Button
                variant="outline"
                size="sm"
                disabled={pair.isFetching}
                onClick={() => void pair.refetch()}
              >
                <RefreshCw aria-hidden="true" />
                {t('setup.phone.newCode')}
              </Button>
            </div>
          </div>
        </div>

        {webPlayer ? <WebPlayerButton /> : null}

        <div className="flex flex-col gap-2 text-[13px]">
          {ffmpeg?.path ? (
            <StatusText tone="ok">{t('setup.phone.ffmpeg')}</StatusText>
          ) : system.data ? (
            <StatusText tone="warn">{t('setup.phone.noFfmpeg')}</StatusText>
          ) : null}
          <span className="inline-flex items-start gap-1.5 text-muted-foreground">
            <Users className="mt-0.5 size-[15px] shrink-0" aria-hidden="true" />
            {t('setup.phone.household')}
          </span>
        </div>
      </div>
      <StepFooter
        back={back}
        onSkip={() => leave('skipped')}
        onContinue={() => leave('done')}
        last={last}
      />
    </>
  );
}

/** Opens the web player in a new tab, signed in as this admin by a token of its own. */
function WebPlayerButton() {
  const { t } = useTranslation();
  const me = useCurrentUser();
  const { busy, open } = usePlayerTab(t('setup.phone.webFailed'));
  return (
    <div className="flex flex-wrap items-center gap-3">
      <Button
        variant="outline"
        disabled={busy}
        onClick={() => void open((p) => playerConnectUrl(p, me.id))}
      >
        <Headphones aria-hidden="true" />
        {t('setup.phone.web')}
      </Button>
      <span className="text-[13px] text-muted-foreground">{t('setup.phone.webBody')}</span>
    </div>
  );
}

/**
 * Step 8: add a Bridge (optional). What it does, the compose snippet, and a
 * pairing code through the same dialog as Settings > Bridge. Once a Bridge is
 * paired, its own setup checklist (F2) shows instead.
 */
export function BridgeStep({ libraries, leave, back, last }: StepProps) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const bridge = useBridge();
  const status = bridge.data;
  const [shown, setShown] = useState<ShownCode | null>(null);
  const [making, setMaking] = useState(false);
  const root = libraries.find((l) => l.kind === 'books')?.root ?? '';
  const compose = bridgeCompose(root);

  const makeCode = async () => {
    setMaking(true);
    try {
      const code = await api.pairBridge();
      setShown({
        ...code,
        pairedAtBefore: status?.paired ? status.paired_at : undefined,
        replacing: Boolean(status?.paired),
      });
    } catch (err) {
      toastError(t('bridge.pair.failed'), err);
    } finally {
      setMaking(false);
      void qc.invalidateQueries({ queryKey: keys.bridge });
    }
  };
  const closeCode = useCallback(() => setShown(null), []);

  return (
    <>
      <div className="flex flex-col gap-5">
        <p>{t('setup.bridge.about')}</p>
        {status?.paired ? (
          <div className="flex flex-col gap-3 rounded-xl border px-4 py-3.5">
            <span className="inline-flex items-center gap-2 font-semibold">
              <CircleCheck className="size-4 text-success" aria-hidden="true" />
              {status.online ? t('setup.bridge.online') : t('setup.bridge.paired')}
            </span>
            {status.setup ? (
              <BridgeSetupList setup={status.setup} uiUrl={status.ui_url} />
            ) : (
              <span className="text-[13px] text-muted-foreground">{t('setup.bridge.waiting')}</span>
            )}
          </div>
        ) : (
          <>
            <div className="flex flex-col gap-2">
              <span className="text-[13px] text-muted-foreground">
                {t('setup.bridge.composeIntro')}
              </span>
              <pre className="max-h-[320px] overflow-auto rounded-lg bg-muted px-3 py-2.5 font-mono text-[12px] leading-5">
                {compose}
              </pre>
              <div className="flex flex-wrap items-center gap-2">
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
                  {t('setup.bridge.agent')}
                </span>
              </div>
            </div>
            <div>
              <Button variant="outline" disabled={making} onClick={() => void makeCode()}>
                {making ? (
                  <LoaderCircle className="animate-spin" aria-hidden="true" />
                ) : (
                  <Link2 aria-hidden="true" />
                )}
                {t('bridge.connect')}
              </Button>
            </div>
          </>
        )}
        {bridge.isError ? (
          <Notice tone="warn" icon={Link2} title={t('bridge.error')} className="mt-1" />
        ) : null}
      </div>
      <StepFooter
        back={back}
        onSkip={() => leave('skipped')}
        onContinue={() => leave(status?.paired ? 'done' : 'skipped')}
        last={last}
      />
      <PairDialog
        shown={shown}
        status={status}
        onClose={closeCode}
        onNewCode={() => void makeCode()}
        making={making}
      />
    </>
  );
}
