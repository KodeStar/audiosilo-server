import { Link, useSearch } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import {
  Archive,
  ArrowRight,
  BellRing,
  Globe,
  Import,
  Info,
  MonitorSmartphone,
  Repeat2,
  RotateCcw,
  Settings2,
  ShieldCheck,
  Ticket,
  TriangleAlert,
  type LucideIcon,
} from 'lucide-react';
import { useLibraries, useSettings, useSystem } from '@/api/hooks';
import type { AdminSettings } from '@/api/types';
import { Notice } from '@/components/notice';
import { Page } from '@/components/page';
import { PageHead } from '@/components/page-head';
import { QueryError } from '@/components/query-error';
import { SettingRow } from '@/components/setting-row';
import { StatusText } from '@/components/status-text';
import { Badge } from '@/components/ui/badge';
import { buttonVariants } from '@/components/ui/button';
import { Card, CardHeader } from '@/components/ui/card';
import { formatDate, formatNumber } from '@/lib/format';
import { regionOptions } from '@/lib/regions';
import { DEFAULT_SERVER_NAME } from '@/lib/server-label';
import { toast } from '@/lib/toast';
import { cn } from '@/lib/utils';
import { activeMirror, mirrorLook } from '@/features/health/system-model';
import { ImportTopic } from '@/features/imports/import-topic';
import { BackupsTopic } from './backups-topic';
import { ClearMatchesZone } from './clear-matches';
import { NotificationsTopic } from './notifications-topic';
import { InstantSwitch, SettingBadges, SettingsForm } from './settings-form';
import {
  certificateLook,
  lockOf,
  METADATA_MODES,
  SETTINGS_PAGES,
  type CertStatus,
  type SettingsPage,
} from './settings-model';

const PAGE_ICONS: Record<SettingsPage, LucideIcon> = {
  general: Settings2,
  network: ShieldCheck,
  players: MonitorSmartphone,
  metadata: Globe,
  transcoding: Repeat2,
  demo: Ticket,
  backups: Archive,
  notifications: BellRing,
  import: Import,
};

/**
 * Server > Settings: config.yaml as a page, one topic at a time (`?topic=`),
 * each setting in exactly one place. A setting the environment or the desktop
 * app sets is shown locked, one read only at start says so, and a saved one
 * still waiting for a restart is listed at the top.
 */
export function SettingsPage() {
  const { t } = useTranslation();
  const { topic = 'general' } = useSearch({ strict: false }) as { topic?: SettingsPage };
  const settings = useSettings();

  return (
    <Page>
      <PageHead title={t('settings.title')} description={t('settings.description')} />
      <div className="grid grid-cols-1 gap-6 md:grid-cols-[210px_minmax(0,1fr)]">
        <nav
          aria-label={t('settings.topics')}
          className="-mx-4 flex gap-1 overflow-x-auto px-4 pb-1 md:mx-0 md:flex-col md:overflow-visible md:p-0"
        >
          {SETTINGS_PAGES.map((p) => {
            const Icon = PAGE_ICONS[p];
            return (
              <Link
                key={p}
                to="/server/{-$section}"
                params={{ section: undefined }}
                search={p === 'general' ? {} : { topic: p }}
                // Exact, so General's empty search isn't read as a subset of every topic's.
                activeOptions={{ exact: true }}
                aria-current={p === topic ? 'page' : undefined}
                className="flex shrink-0 items-center gap-2.5 rounded-md px-3 py-2 text-[13.5px] font-semibold whitespace-nowrap text-muted-foreground transition-colors duration-(--dur-1) hover:bg-accent hover:text-foreground aria-[current=page]:bg-card aria-[current=page]:text-foreground aria-[current=page]:shadow-[inset_0_0_0_1px_var(--border)]"
              >
                <Icon className="size-4" aria-hidden="true" />
                {t(`settings.topic.${p}`)}
              </Link>
            );
          })}
        </nav>
        <div className="flex max-w-[820px] min-w-0 flex-col gap-5">
          <h2 className="h2">{t(`settings.topic.${topic}`)}</h2>
          {settings.isError ? (
            <QueryError
              title={t('settings.error')}
              error={settings.error}
              onRetry={() => void settings.refetch()}
            />
          ) : !settings.data ? (
            <div
              className="skel h-[220px] rounded-xl"
              role="status"
              aria-label={t('common.loading')}
            />
          ) : (
            <>
              <RestartNotice settings={settings.data} />
              <Topic topic={topic} settings={settings.data} />
            </>
          )}
        </div>
      </div>
    </Page>
  );
}

function Topic({ topic, settings }: { topic: SettingsPage; settings: AdminSettings }) {
  switch (topic) {
    case 'network':
      return <NetworkTopic settings={settings} />;
    case 'players':
      return <PlayersTopic settings={settings} />;
    case 'metadata':
      return <MetadataTopic settings={settings} />;
    case 'transcoding':
      return <TranscodingTopic />;
    case 'demo':
      return <DemoTopic settings={settings} />;
    case 'backups':
      return <BackupsTopic settings={settings} />;
    case 'notifications':
      return <NotificationsTopic />;
    case 'import':
      return <ImportTopic />;
    default:
      return <GeneralTopic settings={settings} />;
  }
}

/** Saved settings the running server doesn't use yet. */
function RestartNotice({ settings }: { settings: AdminSettings }) {
  const { t } = useTranslation();
  if (settings.restart_pending.length === 0) return null;
  return (
    <Notice tone="warn" icon={RotateCcw} title={t('settings.restart.title')} role="status">
      {t('settings.restart.body', {
        list: settings.restart_pending.map((id) => t(`settings.${id}`)).join(', '),
      })}
    </Notice>
  );
}

function GeneralTopic({ settings }: { settings: AdminSettings }) {
  const { t } = useTranslation();
  return (
    <>
      <SettingsForm
        settings={settings}
        section="general"
        title={t('settings.general.card')}
        fields={[
          { name: 'name', kind: 'text', placeholder: DEFAULT_SERVER_NAME },
          {
            name: 'public_url',
            kind: 'text',
            mono: true,
            placeholder: 'https://books.example.com',
          },
          {
            name: 'lan_url',
            kind: 'text',
            mono: true,
            placeholder: 'http://192.168.1.20:8080',
          },
        ]}
      />
      <SettingsForm
        settings={settings}
        section="general"
        title={t('settings.general.history')}
        description={t('settings.general.historyBody')}
        fields={[{ name: 'session_days', kind: 'number', placeholder: '400' }]}
      />
      <Card aria-labelledby="updates-title">
        <CardHeader
          titleId="updates-title"
          title={t('settings.updates.title')}
          action={
            <Link
              to="/server/{-$section}"
              params={{ section: 'about' }}
              className={cn(buttonVariants({ variant: 'ghost', size: 'sm' }))}
            >
              {t('settings.updates.about')}
              <ArrowRight aria-hidden="true" />
            </Link>
          }
        />
        <div className="px-5">
          <SettingRow
            title={t('settings.general.update_check')}
            badges={<SettingBadges settings={settings} id="general.update_check" />}
            htmlFor="update-switch"
            description={t('settings.general.update_checkBody')}
            descriptionId="update-switch-desc"
          >
            <InstantSwitch
              id="update-switch"
              checked={settings.general.update_check}
              disabled={Boolean(lockOf(settings, 'general.update_check'))}
              describedBy="update-switch-desc"
              patch={(on) => ({ general: { update_check: on } })}
              failedTitle={t('settings.updates.failed')}
              onSaved={(on) =>
                toast.add({
                  title: on ? t('settings.updates.on') : t('settings.updates.off'),
                  description: on ? undefined : t('settings.updates.offBody'),
                  type: 'success',
                })
              }
            />
          </SettingRow>
        </div>
      </Card>
    </>
  );
}

function NetworkTopic({ settings }: { settings: AdminSettings }) {
  const { t } = useTranslation();
  const modes = ['off', 'selfsigned', 'autocert'] as const;
  return (
    <>
      <SettingsForm
        settings={settings}
        section="network"
        title={t('settings.network.https')}
        description={t('settings.network.httpsBody')}
        confirmRestart
        fields={[
          {
            name: 'tls_mode',
            kind: 'radio',
            options: modes.map((m) => ({
              value: m,
              title: t(`settings.network.mode.${m}`),
              description: t(`settings.network.mode.${m}Body`),
            })),
          },
          { name: 'tls_hosts', kind: 'list', placeholder: 'books.example.com' },
        ]}
        visible={(name, draft) => name !== 'tls_hosts' || draft.tls_mode === 'autocert'}
      >
        <CertificateRow />
      </SettingsForm>
      <SettingsForm
        settings={settings}
        section="network"
        title={t('settings.network.card')}
        confirmRestart
        fields={[
          { name: 'bind', kind: 'text', mono: true, placeholder: '0.0.0.0:8080' },
          { name: 'trusted_proxies', kind: 'list', placeholder: '10.0.0.2/32' },
          { name: 'cors_origins', kind: 'list', placeholder: 'http://localhost:8081' },
        ]}
      />
    </>
  );
}

const CERT_BADGE: Record<CertStatus, 'success' | 'warning' | 'destructive' | 'secondary'> = {
  ok: 'success',
  warn: 'warning',
  bad: 'destructive',
  waiting: 'secondary',
};

/** The served certificate's state, from the system status. */
function CertificateRow() {
  const { t, i18n } = useTranslation();
  const system = useSystem();
  const lang = i18n.resolvedLanguage ?? 'en';
  const tls = system.data?.tls;
  let value: React.ReactNode = <span className="skel inline-block h-5 w-40" />;
  if (system.isError)
    value = <span className="text-muted-foreground">{t('settings.unknown')}</span>;
  else if (tls) {
    const look = certificateLook(tls, Date.now());
    value = look ? (
      <Badge variant={CERT_BADGE[look.status]}>
        {t(look.key, { days: formatNumber(look.days ?? 0, lang), count: look.days ?? 0 })}
      </Badge>
    ) : (
      <span className="text-muted-foreground">{t('settings.network.cert.none')}</span>
    );
  }
  const first = tls?.certificates[0];
  return (
    <SettingRow
      title={t('settings.network.cert.title')}
      description={
        first?.issued
          ? t('settings.network.cert.detail', {
              issuer: first.issuer || t('settings.unknown'),
              date: formatDate(first.not_after, lang),
            })
          : undefined
      }
    >
      {value}
    </SettingRow>
  );
}

function PlayersTopic({ settings }: { settings: AdminSettings }) {
  const { t } = useTranslation();
  const p = settings.players;
  return (
    <>
      <Card aria-labelledby="player-title">
        <CardHeader titleId="player-title" title={t('settings.players.card')} />
        <div className="divide-y px-5">
          <SettingRow
            title={t('settings.players.web_player')}
            description={t(`settings.players.source.${p.web_player || 'none'}Body`)}
          >
            <StatusText tone={p.web_player ? 'ok' : 'off'} className="font-semibold">
              {t(`settings.players.source.${p.web_player || 'none'}`)}
            </StatusText>
          </SettingRow>
          {p.web_dir ? (
            <SettingRow
              title={t('settings.players.web_dir')}
              badges={<SettingBadges settings={settings} id="players.web_dir" />}
              description={t('settings.players.web_dirBody')}
            >
              <span className="font-mono text-[12.5px] [overflow-wrap:anywhere]">{p.web_dir}</span>
            </SettingRow>
          ) : null}
        </div>
      </Card>
      <SettingsForm
        settings={settings}
        section="players"
        title={t('settings.players.links')}
        description={t('settings.players.linksBody')}
        fields={[
          { name: 'apple_app_ids', kind: 'list', placeholder: 'ABCDE12345.app.audiosilo' },
          { name: 'android_package', kind: 'text', mono: true, placeholder: 'app.audiosilo' },
          { name: 'android_sha256', kind: 'list', placeholder: 'AB:CD:…' },
        ]}
      />
    </>
  );
}

function MetadataTopic({ settings }: { settings: AdminSettings }) {
  const { t, i18n } = useTranslation();
  const m = settings.metadata;
  return (
    <>
      <Card aria-labelledby="metadata-title">
        <CardHeader
          titleId="metadata-title"
          title={t('settings.metadata.title')}
          description={t('settings.metadata.description')}
        />
        <div className="divide-y px-5">
          <SettingRow
            title={t('settings.metadata.toggle')}
            badges={<SettingBadges settings={settings} id="metadata.enabled" />}
            htmlFor="metadata-switch"
            description={t('settings.metadata.toggleBody')}
            descriptionId="metadata-switch-desc"
          >
            <InstantSwitch
              id="metadata-switch"
              checked={m.enabled && m.available}
              disabled={!m.available || Boolean(lockOf(settings, 'metadata.enabled'))}
              describedBy="metadata-switch-desc"
              patch={(on) => ({ metadata: { enabled: on } })}
              failedTitle={t('settings.metadata.failed')}
              onSaved={(on) =>
                toast.add({
                  title: on ? t('settings.metadata.on') : t('settings.metadata.off'),
                  description: on ? undefined : t('settings.metadata.offBody'),
                  type: 'success',
                })
              }
            />
          </SettingRow>
          {m.enabled && m.available ? <MetadataStatusRow /> : null}
        </div>
        {!m.available ? (
          <Notice
            tone="warn"
            icon={TriangleAlert}
            title={t('settings.metadata.unavailable')}
            className="mx-5 mb-5"
          >
            {t('settings.metadata.unavailableBody')}
          </Notice>
        ) : null}
      </Card>
      <SettingsForm
        settings={settings}
        section="metadata"
        title={t('settings.metadata.sourceCard')}
        description={t('settings.metadata.sourceCardBody')}
        fields={[
          {
            name: 'mode',
            kind: 'radio',
            columns: 2,
            options: METADATA_MODES.map((mode) => ({
              value: mode,
              title: t(`settings.metadata.mode.${mode}`),
              description: t(`settings.metadata.mode.${mode}Body`),
            })),
          },
        ]}
      />
      <SettingsForm
        settings={settings}
        section="metadata"
        title={t('settings.metadata.matchingCard')}
        description={t('settings.metadata.matchingCardBody')}
        fields={[
          {
            name: 'region',
            kind: 'select',
            options: [
              { value: '', label: t('settings.metadata.regionNone') },
              ...regionOptions(i18n.resolvedLanguage ?? 'en'),
            ],
          },
        ]}
      />
      <SettingsForm
        settings={settings}
        section="metadata"
        title={t('settings.metadata.addressCard')}
        fields={[
          { name: 'base_url', kind: 'text', mono: true, placeholder: 'https://meta.audiosilo.app' },
        ]}
      />
      <ClearMatchesZone />
    </>
  );
}

/**
 * How lookups are going: the service's health, or in mirror mode the local
 * copy's state with a link to its details (Health > System).
 */
function MetadataStatusRow() {
  const { t, i18n } = useTranslation();
  const system = useSystem();
  const h = system.data?.metadata.health;
  const mirror = system.data && activeMirror(system.data);
  if (mirror) {
    return (
      <SettingRow
        title={t('settings.metadata.status')}
        description={
          <Link
            to="/health/{-$section}"
            params={{ section: 'system' }}
            className="font-semibold text-brand-ink hover:underline"
          >
            {t('settings.metadata.mirrorDetails')}
          </Link>
        }
      >
        <StatusText tone={mirrorLook(mirror).status} colored className="font-semibold">
          {t(`settings.metadata.mirror.${mirror.state}`)}
        </StatusText>
      </SettingRow>
    );
  }
  return (
    <SettingRow title={t('settings.metadata.status')}>
      {!h ? (
        <span className="skel inline-block h-5 w-36" />
      ) : (
        <StatusText tone={h.reachable ? 'ok' : 'bad'} colored className="font-semibold">
          {h.reachable
            ? t('settings.metadata.responding', {
                ms: formatNumber(h.latency_ms, i18n.resolvedLanguage ?? 'en'),
              })
            : t('settings.metadata.notResponding')}
        </StatusText>
      )}
    </SettingRow>
  );
}

function TranscodingTopic() {
  const { t } = useTranslation();
  const system = useSystem();
  return (
    <Card aria-labelledby="transcoding-title">
      <CardHeader
        titleId="transcoding-title"
        title={t('settings.transcoding.card')}
        description={t('settings.transcoding.cardBody')}
      />
      <div className="divide-y px-5">
        {system.isError ? (
          <div className="py-4">
            <QueryError
              title={t('system.error')}
              error={system.error}
              onRetry={() => void system.refetch()}
            />
          </div>
        ) : !system.data ? (
          <div className="py-4">
            <span className="skel block h-16" role="status" aria-label={t('common.loading')} />
          </div>
        ) : (
          system.data.tools.map((tool) => (
            <SettingRow
              key={tool.name}
              title={tool.name}
              description={
                tool.path ? (
                  <span className="font-mono [overflow-wrap:anywhere]">{tool.path}</span>
                ) : (
                  t(`settings.transcoding.missing.${tool.name}`)
                )
              }
            >
              {tool.path ? (
                <Badge variant="success">
                  {tool.version || t('settings.transcoding.found')}
                  {tool.source === 'downloaded' ? ` · ${t('settings.transcoding.downloaded')}` : ''}
                </Badge>
              ) : (
                <Badge variant="warning">{t('settings.transcoding.off')}</Badge>
              )}
            </SettingRow>
          ))
        )}
      </div>
      <Notice tone="info" icon={Info} className="mx-5 mb-5">
        {t('settings.transcoding.how')}
      </Notice>
    </Card>
  );
}

function DemoTopic({ settings }: { settings: AdminSettings }) {
  const { t, i18n } = useTranslation();
  const libraries = useLibraries();
  const names = (libraries.data ?? []).map((l) => l.name);
  const current = settings.demo.library;
  const options = [
    { value: '', label: t('settings.demo.noLibrary') },
    ...[...new Set(current ? [current, ...names] : names)].map((n) => ({ value: n, label: n })),
  ];
  return (
    <SettingsForm
      settings={settings}
      section="demo"
      title={t('settings.demo.card')}
      description={t('settings.demo.cardBody')}
      fields={[
        { name: 'enabled', kind: 'switch' },
        { name: 'library', kind: 'select', options },
        {
          name: 'max_users',
          kind: 'number',
          placeholder: t('settings.demo.maxDefault', {
            n: formatNumber(settings.demo.max_users_default, i18n.resolvedLanguage ?? 'en'),
          }),
        },
        { name: 'idle_ttl', kind: 'text', mono: true, placeholder: '24h' },
      ]}
    />
  );
}
