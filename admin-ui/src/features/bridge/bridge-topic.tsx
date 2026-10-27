import { useMemo, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { Link } from '@tanstack/react-router';
import type { TFunction } from 'i18next';
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
import { keys, useSettings } from '@/api/hooks';
import type { BridgeLibrary, BridgeSetup, BridgeStatus, SidecarsEstimates } from '@/api/types';
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
import { formatDateTime, formatList, formatNumber, formatRelative, formatUsd } from '@/lib/format';
import { toast } from '@/lib/toast';
import { cn } from '@/lib/utils';
import { CardEmpty } from '@/features/settings/card-empty';
import { queueJob } from './bridge-actions';
import { BridgeCompose } from './bridge-compose';
import { JobsCard } from './bridge-jobs';
import {
  bridgeTabLink,
  KIND_VERIFY,
  kindName,
  MAPPING_TONE,
  mappingState,
  presence,
  PRESENCE_TONE,
  safeHttpUrl,
} from './bridge-model';
import { PairDialog } from './pair-dialog';
import { BridgeSetupList } from './setup-list';
import {
  ACCOUNTS_TAB,
  agentCredentialSet,
  billingKnown,
  githubStep,
  setupComplete,
  setupProgress,
} from './setup-model';
import { useBridge, usePairCode } from './use-bridge';
import { VerifyDialog } from './verify-dialog';

/**
 * Settings > Bridge: pair the optional companion that writes to the libraries
 * and runs jobs, see what it last reported (online, version, each library's
 * mapping, workers), queue a Verify per library, follow and cancel its jobs, and
 * unpair it. The server never connects to the Bridge; it calls in.
 */
export function BridgeTopic() {
  const { t, i18n } = useTranslation();
  const bridge = useBridge();
  const status = bridge.data;
  const { shown, making, makeCode, closeCode } = usePairCode(status);
  // Each server library's name by id, for the job rows.
  const libraries = status?.libraries;
  const libraryNames = useMemo(
    () => new Map((libraries ?? []).map((l) => [l.id, l.name] as const)),
    [libraries],
  );

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
        <NotPaired
          roots={status.libraries.map((l) => l.root)}
          onPair={() => void makeCode()}
          making={making}
        />
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

/**
 * What the Bridge is, where to get one (the compose snippet the setup flow's
 * Bridge step shows, and Running a Bridge), and Connect a bridge.
 */
function NotPaired({
  roots,
  onPair,
  making,
}: {
  roots: readonly string[];
  onPair: () => void;
  making: boolean;
}) {
  const { t } = useTranslation();
  return (
    <Card aria-labelledby="bridge-title">
      <CardHeader titleId="bridge-title" title={t('bridge.title')} />
      <div className="flex flex-col items-start gap-4 px-5 py-5">
        <p className="max-w-[620px] text-muted-foreground">{t('bridge.about')}</p>
        <div className="w-full">
          <BridgeCompose roots={roots} />
        </div>
        <p className="max-w-[620px] text-[13px] text-muted-foreground">{t('bridge.thenPair')}</p>
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

/**
 * The paired Bridge: its setup checklist (first while a required step is still
 * to do, as the Bridge's own page leads with it, but never above the offline
 * notice: an offline Bridge's checklist is only what it last reported), its status, libraries,
 * workers, jobs, and Unpair.
 */
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
  const setup = status.setup;
  const setupCard = setup ? <SetupCard status={status} setup={setup} /> : null;
  const setupFirst = setup ? presence(status) !== 'offline' && !setupComplete(setup) : false;
  return (
    <>
      {setupFirst ? setupCard : null}
      <StatusCard status={status} onPair={onPair} making={making} />
      {setupFirst ? null : setupCard}
      <LibrariesCard status={status} />
      <WorkersCard status={status} />
      <JobsCard libraryNames={libraryNames} uiUrl={status.ui_url} />
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
  const sidecars = status.estimates?.sidecars;

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
            ...(sidecars ? estimateRows(sidecars, t, lang, billingKnown(status)) : []),
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

/**
 * The Bridge's setup checklist as its last heartbeat sent it (Phase 2b), and
 * its accounts read-only (decision D6): the GitHub account signed in for
 * contributing, or none, and whether the agent's credential is set. Signing in
 * happens only on the Bridge's own page (the server never calls the Bridge), so
 * the card links to its Accounts tab. While the Bridge is offline the card says
 * it is the last report and leaves out whether it can contribute (it can't now).
 */
function SetupCard({ status, setup }: { status: BridgeStatus; setup: BridgeSetup }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const { done, total } = setupProgress(setup);
  const complete = done === total;
  const offline = presence(status) === 'offline';
  const agent = agentCredentialSet(setup);
  const accounts = bridgeTabLink(status.ui_url, ACCOUNTS_TAB);
  // Ready on the Bridge's side, but nothing is offered while the module is off (BRIDGE-07).
  const moduleOff = useSettings().data?.modules.contributions === false;
  return (
    <Card aria-labelledby="bridge-setup-title">
      <CardHeader
        titleId="bridge-setup-title"
        title={complete ? t('bridge.setup.title') : t('bridge.setup.titleTodo')}
        description={
          offline && status.heartbeat_at ? (
            <StatusText tone="off" colored className="font-semibold">
              {t('bridge.setup.lastReported', {
                when: formatRelative(status.heartbeat_at, lang),
              })}
            </StatusText>
          ) : (
            <StatusText tone={complete ? 'ok' : 'warn'} colored className="font-semibold">
              {complete
                ? t('bridge.setup.complete')
                : t('bridge.setup.progress', {
                    done: formatNumber(done, lang),
                    total: formatNumber(total, lang),
                  })}
            </StatusText>
          )
        }
      />
      <div className="flex flex-col gap-4 px-5 py-3">
        <BridgeSetupList setup={setup} uiUrl={status.ui_url} />
        <FactList
          rows={[
            [t('bridge.setup.github'), <GitHubAccount key="github" setup={setup} />],
            ...(agent === undefined
              ? []
              : ([
                  [
                    t('bridge.setup.agentCredential'),
                    <StatusText key="agent" tone={agent ? 'ok' : 'warn'}>
                      {agent ? t('bridge.setup.credentialSet') : t('bridge.setup.credentialNotSet')}
                    </StatusText>,
                  ],
                ] as [string, React.ReactNode][])),
            ...(offline
              ? []
              : ([
                  [
                    t('bridge.setup.contributing'),
                    setup.can_contribute && moduleOff ? (
                      <span key="contribute" className="inline-flex flex-wrap items-center gap-x-2">
                        <StatusText tone="warn">
                          {t('bridge.setup.canContributeModuleOff')}
                        </StatusText>
                        <Link
                          to="/server/{-$section}"
                          params={{ section: undefined }}
                          search={{ topic: 'modules' }}
                          className="text-[12.5px] font-semibold text-brand-ink hover:underline"
                        >
                          {t('bridge.setup.openModules')}
                        </Link>
                      </span>
                    ) : (
                      <StatusText key="contribute" tone={setup.can_contribute ? 'ok' : 'off'}>
                        {setup.can_contribute
                          ? t('bridge.setup.canContribute')
                          : t('bridge.setup.cantContribute')}
                      </StatusText>
                    ),
                  ],
                ] as [string, React.ReactNode][])),
          ]}
        />
      </div>
      <div className="flex flex-wrap items-center justify-between gap-3 border-t px-5 py-3.5">
        <span className="text-[12.5px] text-muted-foreground">
          {t('bridge.setup.accountsBody')}
        </span>
        {accounts ? (
          <a
            href={accounts}
            target="_blank"
            rel="noopener noreferrer"
            className={cn(buttonVariants({ variant: 'outline', size: 'sm' }))}
          >
            {t('bridge.setup.openAccounts')}
            <ExternalLink aria-hidden="true" />
          </a>
        ) : null}
      </div>
    </Card>
  );
}

/**
 * The GitHub account row, agreeing with the checklist's GitHub step: the
 * account's name; or, when the step is done with no name reported (the gh
 * CLI's token, say), signed in, in the Bridge's own words; else not signed in.
 */
function GitHubAccount({ setup }: { setup: BridgeSetup }) {
  const { t } = useTranslation();
  if (setup.github_login) {
    return <span className="font-mono text-[12.5px]">{setup.github_login}</span>;
  }
  const step = githubStep(setup);
  if (step?.done) {
    return (
      <StatusText tone="ok">
        {step.detail
          ? t('bridge.setup.signedInVia', { how: step.detail })
          : t('bridge.setup.signedIn')}
      </StatusText>
    );
  }
  return <StatusText tone="off">{t('bridge.setup.notSignedIn')}</StatusText>;
}

/**
 * What the Bridge's last heartbeat says about characters and recaps: its agent
 * (and whether it's ready), how it is paid for (only once there is a credential
 * to pay with, `billing`), this month's spend against the cap, and how many
 * finished books its estimates come from.
 */
function estimateRows(
  e: SidecarsEstimates,
  t: TFunction,
  lang: string,
  billing: boolean,
): [string, React.ReactNode][] {
  const spent = formatUsd(e.month_spent_usd, lang);
  return [
    [
      t('bridge.estimates.agent'),
      e.agent_backend ? (
        <span className="inline-flex flex-wrap items-center gap-x-2">
          <span className="font-mono text-[12.5px]">{e.agent_backend}</span>
          <StatusText tone={e.agent_ready ? 'ok' : 'warn'}>
            {e.agent_ready ? t('bridge.estimates.ready') : t('bridge.estimates.notReady')}
          </StatusText>
        </span>
      ) : (
        <StatusText tone="off">{t('bridge.estimates.agentNone')}</StatusText>
      ),
    ],
    ...(billing
      ? ([
          [
            t('bridge.estimates.billing'),
            e.subscription ? t('bridge.estimates.subscription') : t('bridge.estimates.apiKey'),
          ],
        ] as [string, React.ReactNode][])
      : []),
    [
      t('bridge.estimates.month'),
      e.monthly_budget_usd > 0
        ? t('bridge.estimates.monthOf', { spent, budget: formatUsd(e.monthly_budget_usd, lang) })
        : t('bridge.estimates.monthNoCap', { spent }),
    ],
    [
      t('bridge.estimates.history'),
      e.samples > 0 ? (
        t('bridge.estimates.historyBooks', { count: e.samples, n: formatNumber(e.samples, lang) })
      ) : (
        <span className="text-muted-foreground">{t('bridge.estimates.historyNone')}</span>
      ),
    ],
  ];
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
              <dt className="text-muted-foreground">
                {l.kind === 'podcasts'
                  ? t('bridge.mapping.checkedMarker')
                  : t('bridge.mapping.checked')}
              </dt>
              <dd className="font-mono text-[12px] [overflow-wrap:anywhere]">{m.checked_path}</dd>
            </>
          ) : null}
        </dl>
        {m?.error ? (
          <p className="text-[12.5px] text-destructive [overflow-wrap:anywhere]">{m.error}</p>
        ) : null}
      </div>
      {l.kind === 'podcasts' ? null : (
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
      )}
    </li>
  );
}

/** The ASR workers the Bridge reports (Phase 1b), or that none has connected yet. */
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
