import { useState } from 'react';
import { Link } from '@tanstack/react-router';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import {
  BookOpen,
  CheckCircle2,
  ExternalLink,
  Info,
  Package,
  PowerOff,
  RotateCw,
  TriangleAlert,
} from 'lucide-react';
import { api } from '@/api/client';
import { keys, useSystem } from '@/api/hooks';
import type { SystemStatus, UpdateStatus } from '@/api/types';
import { FactList } from '@/components/fact-list';
import { Notice } from '@/components/notice';
import { Page } from '@/components/page';
import { PageHead } from '@/components/page-head';
import { QueryError } from '@/components/query-error';
import { Button, buttonVariants } from '@/components/ui/button';
import { Card, CardHeader } from '@/components/ui/card';
import { uptime, schemaNumber } from '@/features/health/system-model';
import { toastError } from '@/lib/errors';
import { formatBytes, formatDate, formatRelative, formatVersion } from '@/lib/format';
import { cn } from '@/lib/utils';

/** Where the project lives; About links to its docs, releases and issues. */
const PROJECT_LINKS = {
  docs: 'https://docs.audiosilo.app',
  source: 'https://github.com/KodeStar/audiosilo-server',
  issues: 'https://github.com/KodeStar/audiosilo-server/issues',
};

/**
 * Server > About: whether a newer AudioSilo exists (and how to get it on this
 * install), then what this server is: version, platform, uptime, data folder,
 * database.
 */
export function AboutPage() {
  const { t } = useTranslation();
  const system = useSystem();
  return (
    <Page>
      <PageHead title={t('about.title')} description={t('about.description')} />
      <div className="flex max-w-[820px] flex-col gap-5">
        {system.isError ? (
          <QueryError
            title={t('system.error')}
            error={system.error}
            onRetry={() => void system.refetch()}
          />
        ) : !system.data ? (
          <div
            className="skel h-[320px] rounded-xl"
            role="status"
            aria-label={t('common.loading')}
          />
        ) : (
          <>
            <UpdateCard update={system.data.update} />
            <ServerFacts sys={system.data} />
          </>
        )}
      </div>
    </Page>
  );
}

function UpdateCard({ update }: { update: UpdateStatus }) {
  const { t, i18n } = useTranslation();
  const qc = useQueryClient();
  const lang = i18n.resolvedLanguage ?? 'en';
  const [checking, setChecking] = useState(false);

  const check = async () => {
    setChecking(true);
    try {
      const next = await api.checkForUpdate();
      qc.setQueryData<SystemStatus>(keys.system, (s) => (s ? { ...s, update: next } : s));
      qc.setQueryData(keys.update, next);
    } catch (err) {
      toastError(t('about.checkFailed'), err);
    } finally {
      setChecking(false);
    }
  };

  const latest = update.latest;
  let notice: React.ReactNode;
  if (!update.enabled) {
    notice = (
      <Notice
        tone="info"
        icon={PowerOff}
        title={t('about.off.title')}
        actions={
          <Link
            to="/server/{-$section}"
            params={{ section: undefined }}
            className={cn(buttonVariants({ variant: 'outline', size: 'sm' }))}
          >
            {t('about.off.action')}
          </Link>
        }
      >
        {t('about.off.body')}
      </Notice>
    );
  } else if (update.update_available && latest) {
    notice = (
      <Notice
        tone="info"
        icon={Package}
        title={t('about.available.title', { version: formatVersion(latest.version) })}
        actions={
          <a
            href={latest.url}
            target="_blank"
            rel="noreferrer noopener"
            className={cn(buttonVariants({ size: 'sm' }))}
          >
            {t('about.available.notes')}
            <ExternalLink aria-hidden="true" />
          </a>
        }
      >
        {/* Image tags and release files name the version without its v. */}
        {t(`about.how.${update.install}`, { version: latest.version.replace(/^v/, '') })}
      </Notice>
    );
  } else if (update.error) {
    notice = (
      <Notice tone="warn" icon={TriangleAlert} title={t('about.failed.title')}>
        {t(`about.error.${update.error}`)}
      </Notice>
    );
  } else if (!update.comparable && latest) {
    notice = (
      <Notice tone="info" icon={Info} title={t('about.dev.title')}>
        {t('about.dev.body', { version: formatVersion(latest.version) })}
      </Notice>
    );
  } else if (latest) {
    notice = (
      <Notice tone="safe" icon={CheckCircle2} title={t('about.current.title')}>
        {t('about.current.body', { version: formatVersion(update.current) })}
      </Notice>
    );
  } else {
    notice = (
      <Notice tone="info" icon={Info} title={t('about.notChecked.title')}>
        {t('about.notChecked.body')}
      </Notice>
    );
  }

  return (
    <Card aria-labelledby="update-title">
      <CardHeader
        titleId="update-title"
        title={t('about.updates')}
        description={
          update.checked_at
            ? t('about.checkedAt', { when: formatRelative(update.checked_at, lang) })
            : undefined
        }
        action={
          update.enabled ? (
            <Button variant="outline" size="sm" onClick={() => void check()} disabled={checking}>
              <RotateCw className={cn(checking && 'animate-spin')} aria-hidden="true" />
              {checking ? t('common.checking') : t('common.checkNow')}
            </Button>
          ) : null
        }
      />
      <div className="p-5">{notice}</div>
      {latest ? (
        <p className="border-t px-5 py-3 text-[12.5px] text-muted-foreground">
          {t('about.latest', {
            version: formatVersion(latest.version),
            date: formatDate(latest.published_at, lang),
          })}
        </p>
      ) : null}
    </Card>
  );
}

function ServerFacts({ sys }: { sys: SystemStatus }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const [n, unit] = uptime(sys.started_at);
  const rows: [string, React.ReactNode][] = [
    [t('about.fact.name'), sys.name],
    [t('about.fact.version'), formatVersion(sys.version)],
    [t('about.fact.install'), t(`about.install.${sys.install}`)],
    [t('about.fact.platform'), `${sys.os}/${sys.arch} · ${sys.go_version}`],
    [t('about.fact.uptime'), t(`about.uptime.${unit}`, { count: n })],
    [t('about.fact.dataDir'), <span className="font-mono text-[12.5px]">{sys.data_dir}</span>],
    [
      t('about.fact.database'),
      t('about.databaseValue', {
        size: formatBytes(sys.database.bytes, lang),
        schema: schemaNumber(sys.database.schema),
      }),
    ],
    [t('about.fact.serverId'), <span className="font-mono text-[12.5px]">{sys.server_id}</span>],
  ];
  return (
    <Card aria-labelledby="facts-title">
      <CardHeader titleId="facts-title" title={t('about.server', { name: sys.name })} />
      <div className="p-5">
        <FactList rows={rows} />
      </div>
      <div className="flex flex-wrap gap-2 border-t px-5 py-3.5">
        {(
          [
            ['docs', BookOpen],
            ['source', Package],
            ['issues', TriangleAlert],
          ] as const
        ).map(([k, Icon]) => (
          <a
            key={k}
            href={PROJECT_LINKS[k]}
            target="_blank"
            rel="noreferrer noopener"
            className={cn(buttonVariants({ variant: 'ghost', size: 'sm' }))}
          >
            <Icon aria-hidden="true" />
            {t(`about.link.${k}`)}
          </a>
        ))}
      </div>
    </Card>
  );
}
