import { useCallback, useMemo, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import {
  Cpu,
  ExternalLink,
  KeyRound,
  Link2,
  LoaderCircle,
  ScanSearch,
  SlidersHorizontal,
  Unlink,
  Waypoints,
} from 'lucide-react';
import { api } from '@/api/client';
import { keys } from '@/api/hooks';
import type { BridgeLibrary, BridgeStatus } from '@/api/types';
import { ConfirmDialog } from '@/components/confirm-dialog';
import { DangerZone } from '@/components/danger-zone';
import { FactList } from '@/components/fact-list';
import { Notice } from '@/components/notice';
import { QueryError } from '@/components/query-error';
import { SettingRow } from '@/components/setting-row';
import { StatusText } from '@/components/status-text';
import { Badge } from '@/components/ui/badge';
import { Button, buttonVariants } from '@/components/ui/button';
import { Card, CardHeader } from '@/components/ui/card';
import { toastError } from '@/lib/errors';
import { formatDateTime, formatList, formatRelative } from '@/lib/format';
import { toast } from '@/lib/toast';
import { cn } from '@/lib/utils';
import { CardEmpty } from '@/features/settings/card-empty';
import { queueJob } from './bridge-actions';
import { JobsCard } from './bridge-jobs';
import {
  KIND_VERIFY,
  kindName,
  MAPPING_TONE,
  mappingState,
  presence,
  PRESENCE_TONE,
  safeHttpUrl,
} from './bridge-model';
import { PairDialog, type ShownCode } from './pair-dialog';
import { useBridge } from './use-bridge';
import { VerifyDialog } from './verify-dialog';

/**
 * Settings > Bridge: pair the optional companion that writes to the libraries
 * and runs jobs, see what it last reported (online, version, each library's
 * mapping, workers), queue a Verify per library, follow and cancel its jobs, and
 * unpair it. The server never connects to the Bridge; it calls in.
 */
export function BridgeTopic() {
  const { t, i18n } = useTranslation();
  const qc = useQueryClient();
  const bridge = useBridge();
  const status = bridge.data;
  const [shown, setShown] = useState<ShownCode | null>(null);
  const [making, setMaking] = useState(false);
  // Each server library's name by id, for the job rows.
  const libraries = status?.libraries;
  const libraryNames = useMemo(
    () => new Map((libraries ?? []).map((l) => [l.id, l.name] as const)),
    [libraries],
  );

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

  if (bridge.isError)
    return (
      <QueryError
        title={t('bridge.error')}
        error={bridge.error}
        onRetry={() => void bridge.refetch()}
      />
    );
  if (!status)
    return (
      <div className="skel h-[220px] rounded-xl" role="status" aria-label={t('common.loading')} />
    );

  return (
    <>
      {status.paired ? (
        <Paired
          status={status}
          libraryNames={libraryNames}
          onPair={() => void makeCode()}
          making={making}
        />
      ) : (
        <NotPaired onPair={() => void makeCode()} making={making} />
      )}
      {status.pairing && !shown ? (
        <Notice
          tone="info"
          icon={KeyRound}
          role="status"
          title={t('bridge.waitingCode.title')}
          actions={
            <Button variant="outline" size="sm" disabled={making} onClick={() => void makeCode()}>
              {t('bridge.pair.newCode')}
            </Button>
          }
        >
          {t('bridge.waitingCode.body', {
            when: formatRelative(status.pairing.expires_at, i18n.resolvedLanguage ?? 'en'),
          })}
        </Notice>
      ) : null}
      {/* Jobs while no Bridge is paired: queued ones survive an unpair and wait for
          the next Bridge, so they stay visible (and cancellable). Hidden when none. */}
      {status.paired ? null : <JobsCard libraryNames={libraryNames} hideWhenEmpty />}
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

/** What the Bridge is, and Connect a bridge. */
function NotPaired({ onPair, making }: { onPair: () => void; making: boolean }) {
  const { t } = useTranslation();
  return (
    <Card aria-labelledby="bridge-title">
      <CardHeader titleId="bridge-title" title={t('bridge.title')} />
      <div className="flex flex-col items-start gap-4 px-5 py-5">
        <p className="max-w-[620px] text-muted-foreground">{t('bridge.about')}</p>
        <Button onClick={onPair} disabled={making}>
          {making ? (
            <LoaderCircle className="animate-spin" aria-hidden="true" />
          ) : (
            <Link2 aria-hidden="true" />
          )}
          {t('bridge.connect')}
        </Button>
      </div>
    </Card>
  );
}

/** The paired Bridge: its status, libraries, workers, jobs, and Unpair. */
function Paired({
  status,
  libraryNames,
  onPair,
  making,
}: {
  status: BridgeStatus;
  libraryNames: ReadonlyMap<number, string>;
  onPair: () => void;
  making: boolean;
}) {
  return (
    <>
      <StatusCard status={status} onPair={onPair} making={making} />
      <LibrariesCard status={status} />
      <WorkersCard status={status} />
      <JobsCard libraryNames={libraryNames} />
      <UnpairZone />
    </>
  );
}

function StatusCard({
  status,
  onPair,
  making,
}: {
  status: BridgeStatus;
  onPair: () => void;
  making: boolean;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const p = presence(status);
  const link = safeHttpUrl(status.ui_url);
  const heard = status.heartbeat_at ? formatRelative(status.heartbeat_at, lang) : undefined;
  const kinds = status.kinds.map((k) => kindName(t, k));

  return (
    <Card aria-labelledby="bridge-title">
      <CardHeader
        titleId="bridge-title"
        title={t('bridge.title')}
        description={
          <StatusText tone={PRESENCE_TONE[p]} colored className="font-semibold">
            {t(`bridge.presence.${p}`)}
          </StatusText>
        }
        action={
          link ? (
            <a
              href={link}
              target="_blank"
              rel="noopener noreferrer"
              className={cn(buttonVariants({ variant: 'outline', size: 'sm' }))}
            >
              {t('bridge.openPage')}
              <ExternalLink aria-hidden="true" />
            </a>
          ) : undefined
        }
      />
      <div className="px-5 py-4">
        <FactList
          rows={[
            [
              t('bridge.status.heard'),
              heard ?? <span className="text-muted-foreground">{t('bridge.status.notYet')}</span>,
            ],
            [
              t('bridge.status.version'),
              status.version ? (
                <span className="font-mono text-[12.5px]">{status.version}</span>
              ) : (
                <span className="text-muted-foreground">{t('bridge.status.notYet')}</span>
              ),
            ],
            [
              t('bridge.status.paired'),
              status.paired_at
                ? status.paired_by
                  ? t('bridge.status.pairedBy', {
                      when: formatDateTime(status.paired_at, lang, true),
                      name: status.paired_by.username,
                    })
                  : formatDateTime(status.paired_at, lang, true)
                : '',
            ],
            [
              t('bridge.status.runs'),
              kinds.length ? (
                formatList(kinds, lang)
              ) : (
                <span className="text-muted-foreground">{t('bridge.status.notYet')}</span>
              ),
            ],
            [
              t('bridge.status.features'),
              status.features.length ? (
                <span className="flex flex-wrap gap-1.5">
                  {status.features.map((f) => (
                    <Badge key={f} variant="outline" className="font-mono">
                      {f}
                    </Badge>
                  ))}
                </span>
              ) : (
                <span className="text-muted-foreground">{t('bridge.status.notYet')}</span>
              ),
            ],
          ]}
        />
      </div>
      {p === 'offline' ? (
        <Notice
          tone="warn"
          icon={Waypoints}
          className="mx-5 mb-5"
          title={t('bridge.offline.title')}
        >
          {t('bridge.offline.body')}
        </Notice>
      ) : null}
      <div className="flex flex-wrap items-center justify-between gap-3 border-t px-5 py-3.5">
        <span className="text-[12.5px] text-muted-foreground">{t('bridge.pairAnotherBody')}</span>
        <Button variant="ghost" size="sm" disabled={making} onClick={onPair}>
          <Link2 aria-hidden="true" />
          {t('bridge.pairAnother')}
        </Button>
      </div>
    </Card>
  );
}

/** Each server library as the Bridge maps it, with Verify. */
function LibrariesCard({ status }: { status: BridgeStatus }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [queueing, setQueueing] = useState<number | null>(null);
  const [options, setOptions] = useState<BridgeLibrary | null>(null);

  const verify = async (l: BridgeLibrary) => {
    setQueueing(l.id);
    await queueJob(
      qc,
      { kind: KIND_VERIFY, library_id: l.id },
      { name: l.name, online: status.online },
    );
    setQueueing(null);
  };

  return (
    <Card aria-labelledby="bridge-libraries-title">
      <CardHeader
        titleId="bridge-libraries-title"
        title={t('bridge.libraries.title')}
        description={t('bridge.libraries.description')}
      />
      {status.libraries.length === 0 ? (
        <CardEmpty
          icon={ScanSearch}
          title={t('bridge.libraries.none')}
          body={t('bridge.libraries.noneBody')}
        />
      ) : (
        <ul className="flex flex-col divide-y" aria-label={t('bridge.libraries.title')}>
          {status.libraries.map((l) => (
            <LibraryRow
              key={l.id}
              library={l}
              busy={queueing === l.id}
              onVerify={() => void verify(l)}
              onOptions={() => setOptions(l)}
            />
          ))}
        </ul>
      )}
      <VerifyDialog library={options} online={status.online} onClose={() => setOptions(null)} />
    </Card>
  );
}

function LibraryRow({
  library: l,
  busy,
  onVerify,
  onOptions,
}: {
  library: BridgeLibrary;
  busy: boolean;
  onVerify: () => void;
  onOptions: () => void;
}) {
  const { t } = useTranslation();
  const m = l.mapping;
  const state = mappingState(m);
  return (
    <li className="flex flex-wrap items-start gap-x-3 gap-y-2 px-5 py-3.5">
      <div className="flex min-w-0 flex-1 basis-64 flex-col gap-1">
        <span className="flex flex-wrap items-center gap-2">
          <b className="font-semibold">{l.name}</b>
          <StatusText tone={MAPPING_TONE[state]} colored className="text-[12.5px] font-semibold">
            {t(`bridge.mapping.${state}`)}
          </StatusText>
        </span>
        {m ? (
          <span className="flex flex-wrap gap-1.5">
            <Badge variant={m.mapped ? 'success' : 'destructive'}>
              {m.mapped ? t('bridge.mapping.mapped') : t('bridge.mapping.notMappedBadge')}
            </Badge>
            <Badge variant={m.writable ? 'success' : 'warning'}>
              {m.writable ? t('bridge.mapping.writable') : t('bridge.mapping.notWritable')}
            </Badge>
          </span>
        ) : null}
        <dl className="grid grid-cols-[auto_minmax(0,1fr)] gap-x-3 gap-y-0.5 text-[12.5px]">
          <dt className="text-muted-foreground">{t('bridge.mapping.serverRoot')}</dt>
          <dd className="font-mono text-[12px] [overflow-wrap:anywhere]">{l.root}</dd>
          <dt className="text-muted-foreground">{t('bridge.mapping.mount')}</dt>
          <dd className="font-mono text-[12px] [overflow-wrap:anywhere]">
            {m?.mount_path || (
              <span className="font-sans text-muted-foreground">
                {t('bridge.mapping.notReported')}
              </span>
            )}
          </dd>
          {m?.checked_path ? (
            <>
              <dt className="text-muted-foreground">{t('bridge.mapping.checked')}</dt>
              <dd className="font-mono text-[12px] [overflow-wrap:anywhere]">{m.checked_path}</dd>
            </>
          ) : null}
        </dl>
        {m?.error ? (
          <p className="text-[12.5px] text-destructive [overflow-wrap:anywhere]">{m.error}</p>
        ) : null}
      </div>
      <div className="flex items-center gap-1">
        <Button variant="outline" size="sm" disabled={busy} onClick={onVerify}>
          {busy ? (
            <LoaderCircle className="animate-spin" aria-hidden="true" />
          ) : (
            <ScanSearch aria-hidden="true" />
          )}
          {t('bridge.verify.action')}
        </Button>
        <Button
          variant="ghost"
          size="icon-sm"
          onClick={onOptions}
          aria-label={t('bridge.verify.optionsAria', { name: l.name })}
        >
          <SlidersHorizontal aria-hidden="true" />
        </Button>
      </div>
    </li>
  );
}

/** The ASR workers the Bridge reports (Phase 1b), or that they come later. */
function WorkersCard({ status }: { status: BridgeStatus }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  return (
    <Card aria-labelledby="bridge-workers-title">
      <CardHeader titleId="bridge-workers-title" title={t('bridge.workers.title')} />
      {status.workers.length === 0 ? (
        <CardEmpty
          icon={Cpu}
          title={t('bridge.workers.none')}
          body={t('bridge.workers.noneBody')}
        />
      ) : (
        <ul className="flex flex-col divide-y" aria-label={t('bridge.workers.title')}>
          {status.workers.map((w, i) => (
            <li
              key={`${i}-${w.name}`}
              className="flex flex-wrap items-center gap-x-3 gap-y-1 px-5 py-3"
            >
              <Cpu className="size-4 shrink-0 text-subtle-foreground" aria-hidden="true" />
              <div className="flex min-w-0 flex-1 basis-48 flex-col">
                <b className="font-semibold [overflow-wrap:anywhere]">{w.name}</b>
                <span className="text-[12.5px] text-muted-foreground">
                  {[w.platform, w.version].filter(Boolean).join(' · ')}
                </span>
              </div>
              {w.last_seen ? (
                <span className="text-[12.5px] text-muted-foreground">
                  {formatRelative(w.last_seen, lang) || w.last_seen}
                </span>
              ) : null}
            </li>
          ))}
        </ul>
      )}
    </Card>
  );
}

/** Unpair: the Bridge's token is revoked; its jobs stay for the next one. */
function UnpairZone() {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [confirming, setConfirming] = useState(false);
  return (
    <DangerZone>
      <SettingRow title={t('bridge.unpair.title')} description={t('bridge.unpair.body')}>
        <Button variant="destructive-outline" onClick={() => setConfirming(true)}>
          <Unlink aria-hidden="true" />
          {t('bridge.unpair.action')}
        </Button>
      </SettingRow>
      <ConfirmDialog
        open={confirming}
        onOpenChange={setConfirming}
        icon={Unlink}
        title={t('bridge.unpair.confirmTitle')}
        description={t('bridge.unpair.confirmBody')}
        confirmLabel={t('bridge.unpair.confirm')}
        onConfirm={async () => {
          await api.unpairBridge();
          toast.add({ title: t('bridge.unpair.done'), type: 'success' });
          void qc.invalidateQueries({ queryKey: keys.bridge });
          void qc.invalidateQueries({ queryKey: keys.bridgeJobLists });
        }}
      >
        <ul className="ml-5 flex list-disc flex-col gap-1 text-[13px] text-muted-foreground">
          <li>{t('bridge.unpair.point.stops')}</li>
          <li>{t('bridge.unpair.point.jobs')}</li>
          <li>{t('bridge.unpair.point.again')}</li>
        </ul>
      </ConfirmDialog>
    </DangerZone>
  );
}
