import { Link } from '@tanstack/react-router';
import { useQuery } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import {
  CircleCheck,
  ExternalLink,
  Headphones,
  Link2,
  LoaderCircle,
  RefreshCw,
  TriangleAlert,
  Users,
} from 'lucide-react';
import { api } from '@/api/client';
import { keys, useServerInfo, useSystem } from '@/api/hooks';
import type { PairToken } from '@/api/types';
import { Notice } from '@/components/notice';
import { QrCode } from '@/components/qr-code';
import { StatusText } from '@/components/status-text';
import { Button, buttonVariants } from '@/components/ui/button';
import { BridgeCompose } from '@/features/bridge/bridge-compose';
import { PairDialog } from '@/features/bridge/pair-dialog';
import { BridgeSetupList } from '@/features/bridge/setup-list';
import { useBridge, usePairCode } from '@/features/bridge/use-bridge';
import { playerConnectUrl } from '@/features/book/book-model';
import { useCurrentUser } from '@/lib/session';
import { usePlayerTab } from '@/lib/use-player-tab';
import { cn } from '@/lib/utils';
import { phoneLink } from './setup-model';
import { StepFooter } from './step-parts';
import type { StepProps } from './step-actions';

/**
 * Step 5: coming from Audiobookshelf. Points to the listening import (in a new
 * tab, so the flow stays open) and says ABS metadata.json files are read. An
 * info-only step: Continue records it done (someone who never used
 * Audiobookshelf has nothing left to do here), Skip skipped.
 */
export function AbsStep({ leave, back, last }: StepProps) {
  const { t } = useTranslation();
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
          >
            <ExternalLink aria-hidden="true" />
            {t('setup.abs.open')}
          </Link>
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
export function PhoneStep({ settings, leave, back, last }: StepProps) {
  const { t } = useTranslation();
  const server = useServerInfo();
  const system = useSystem();
  const pair = useQuery({
    queryKey: keys.setupPair,
    queryFn: () => api.pairSelf(),
    staleTime: Infinity,
    gcTime: 0,
    retry: false,
  });
  const ffmpeg = system.data?.tools.find((x) => x.name === 'ffmpeg');
  const webPlayer = server.data?.capabilities.web_player ?? false;
  const link = pair.data
    ? phoneLink(connectUrl(pair.data), pair.data.addresses?.home || settings.general.lan_url)
    : null;
  // A loopback link: a phone can't open it, so the address action leads and the
  // QR is dimmed (ADMIN-07).
  const loopback = link?.via === 'loopback';

  return (
    <>
      <div className="flex flex-col gap-5">
        {loopback ? (
          <Notice
            tone="warn"
            icon={TriangleAlert}
            role="status"
            title={t('setup.phone.loopback', { address: window.location.host })}
            actions={
              <Link
                to="/setup"
                search={{ step: 'reach' }}
                className={buttonVariants({ size: 'sm' })}
              >
                {t('setup.phone.loopbackAction')}
              </Link>
            }
          >
            {t('setup.phone.loopbackBody')}
          </Notice>
        ) : null}

        <div className="flex flex-wrap items-center gap-5 rounded-xl border px-4 py-4">
          {link ? (
            // Only the code is dimmed: New code beside it still works, and dimmed it
            // would read as disabled.
            <div className={cn('shrink-0', loopback && 'opacity-45')}>
              <QrCode
                text={link.url}
                label={t(loopback ? 'setup.phone.qrUnreachable' : 'setup.phone.qr')}
              />
            </div>
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
            {link?.via === 'home' ? (
              <span className="text-[13px] text-muted-foreground" role="status">
                {t('setup.phone.viaHome', { address: new URL(link.url).origin })}
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
  const bridge = useBridge();
  const status = bridge.data;
  const { shown, making, makeCode, closeCode } = usePairCode(status);

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
            <BridgeCompose roots={libraries.map((l) => l.root)} />
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
