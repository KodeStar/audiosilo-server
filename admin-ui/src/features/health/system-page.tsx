import { useState } from 'react';
import { Link } from '@tanstack/react-router';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import {
  Archive,
  Cpu,
  Database,
  Globe,
  HardDrive,
  MonitorSmartphone,
  Package,
  RotateCw,
  ShieldCheck,
  TriangleAlert,
  Unplug,
  type LucideIcon,
} from 'lucide-react';
import { checkMetaMirror, useMirrorPoll, useSystem } from '@/api/hooks';
import type { MetaMirrorStatus, SystemStatus } from '@/api/types';
import { FactList } from '@/components/fact-list';
import { Notice } from '@/components/notice';
import { Page } from '@/components/page';
import { PageHead } from '@/components/page-head';
import { ProgressBar } from '@/components/progress-bar';
import { QueryError } from '@/components/query-error';
import { StatusText } from '@/components/status-text';
import { Button, buttonVariants } from '@/components/ui/button';
import { toastError } from '@/lib/errors';
import {
  formatBytes,
  formatDate,
  formatDateTime,
  formatNumber,
  formatPercent,
  formatRelative,
  hostOf,
} from '@/lib/format';
import { DEFAULT_SERVER_NAME } from '@/lib/server-label';
import { toast } from '@/lib/toast';
import { cn } from '@/lib/utils';
import {
  activeMirror,
  metadataDown,
  mirrorLook,
  systemRows,
  type MirrorFact,
  type SystemRow,
} from './system-model';

const ICONS: Record<SystemRow['kind'], LucideIcon> = {
  ffmpeg: Cpu,
  ffprobe: Cpu,
  metadata: Globe,
  tls: ShieldCheck,
  database: Database,
  backups: Archive,
  library: HardDrive,
  player: MonitorSmartphone,
  update: Package,
};

/**
 * Health > System: everything the server depends on in one place (tools, the
 * community metadata service, the certificate, the database, each library's
 * disk, the web player, updates), each with a plain status. Polled every 30 s,
 * and the local metadata copy every 2 s while it is busy.
 */
export function SystemPage() {
  const { t } = useTranslation();
  const system = useSystem({ poll: true });
  const sys = system.data;
  useMirrorPoll(sys && activeMirror(sys));
  return (
    <Page>
      <PageHead
        title={t('system.title')}
        description={t('system.description', { name: sys?.name ?? DEFAULT_SERVER_NAME })}
      />
      {system.isError ? (
        <QueryError
          title={t('system.error')}
          error={system.error}
          onRetry={() => void system.refetch()}
        />
      ) : !sys ? (
        <div className="skel h-[420px] rounded-xl" role="status" aria-label={t('common.loading')} />
      ) : (
        <SystemList sys={sys} />
      )}
    </Page>
  );
}

function SystemList({ sys }: { sys: SystemStatus }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const rows = systemRows(sys);
  const offline = sys.libraries.filter((l) => !l.available);
  const metaDown = metadataDown(sys);

  const values = (v?: Record<string, string | number>) =>
    v &&
    Object.fromEntries(
      Object.entries(v).map(([k, x]) => [
        k,
        k === 'at' || k === 'next'
          ? formatDateTime(String(x), lang)
          : typeof x !== 'number' || k === 'count'
            ? x
            : k === 'free' || k === 'total'
              ? formatBytes(x, lang)
              : formatNumber(x, lang),
      ]),
    );

  return (
    <div className="flex flex-col gap-4">
      {offline.length ? (
        <Notice
          tone="safe"
          icon={Unplug}
          title={t('system.offline.title', { count: offline.length })}
        >
          {t('system.offline.body')}
        </Notice>
      ) : null}
      {metaDown ? (
        <Notice
          tone="warn"
          icon={Globe}
          title={t('system.metaDown.title')}
          actions={
            <Link
              to="/server/{-$section}"
              params={{ section: undefined }}
              search={{ topic: 'metadata' }}
              className={cn(buttonVariants({ variant: 'outline', size: 'sm' }))}
            >
              {t('system.metaDown.action')}
            </Link>
          }
        >
          {t('system.metaDown.body')}
        </Notice>
      ) : null}
      <section className="rounded-xl border bg-card" aria-label={t('system.title')}>
        <ul className="flex flex-col divide-y">
          {rows.map((r) => {
            const Icon = ICONS[r.kind];
            return (
              <li
                key={r.id}
                className="flex flex-wrap items-center gap-x-3.5 gap-y-1.5 px-[18px] py-3.5"
              >
                <span
                  className="grid size-[34px] shrink-0 place-items-center rounded-[10px] bg-muted text-muted-foreground"
                  aria-hidden="true"
                >
                  <Icon className="size-[17px]" />
                </span>
                <div className="flex min-w-0 flex-1 basis-56 flex-col gap-0.5">
                  <b className="font-semibold [overflow-wrap:anywhere]">
                    {t(r.title.key, r.title.values)}
                  </b>
                  <span className="text-[12.5px] text-muted-foreground">
                    {t(r.detail.key, values(r.detail.values))}
                  </span>
                </div>
                {r.value ? (
                  <span
                    className="max-w-[260px] truncate font-mono text-[12.5px] text-muted-foreground max-md:hidden"
                    title={r.value}
                  >
                    {r.value}
                  </span>
                ) : null}
                <StatusText
                  tone={r.status}
                  colored
                  className="min-w-[124px] justify-end text-[13px] font-semibold max-md:min-w-0"
                >
                  {t(r.statusKey)}
                </StatusText>
                {r.mirror ? (
                  <MirrorPanel mirror={r.mirror} baseUrl={sys.metadata.base_url} />
                ) : null}
              </li>
            );
          })}
        </ul>
      </section>
    </div>
  );
}

/**
 * The local metadata copy under its row (mirror mode): a download's progress,
 * what the copy is, why lookups go online, a copy newer than this server, the
 * last failure, and "Check now" (not while a download runs or a copy opens).
 */
function MirrorPanel({ mirror, baseUrl }: { mirror: MetaMirrorStatus; baseUrl: string }) {
  const { t, i18n } = useTranslation();
  const look = mirrorLook(mirror);
  const lang = i18n.resolvedLanguage ?? 'en';
  const qc = useQueryClient();
  const [checking, setChecking] = useState(false);

  const check = async () => {
    setChecking(true);
    try {
      await checkMetaMirror(qc);
      toast.add({ title: t('system.mirror.checkStarted'), type: 'success' });
    } catch (err) {
      toastError(t('system.mirror.checkFailed'), err);
    } finally {
      setChecking(false);
    }
  };

  const show = (f: MirrorFact): React.ReactNode => {
    switch (f.kind) {
      case 'mono':
        return <span className="font-mono text-[12.5px]">{f.value}</span>;
      case 'number':
        return formatNumber(Number(f.value), lang);
      case 'bytes':
        return formatBytes(Number(f.value), lang);
      case 'date':
        return formatDate(String(f.value), lang);
      case 'text':
        return t(String(f.value));
      default:
        return (
          <time dateTime={String(f.value)} title={formatDateTime(String(f.value), lang, true)}>
            {formatRelative(String(f.value), lang)}
          </time>
        );
    }
  };

  const p = look.progress;
  return (
    <div className="flex basis-full flex-col gap-3 pt-1 md:pl-[48.5px]">
      {p ? (
        <div className="flex flex-col gap-1.5">
          <ProgressBar fraction={p.fraction} label={t('system.mirror.progressLabel')} />
          <span className="text-[12.5px] text-muted-foreground tabular-nums">
            {p.fraction === undefined
              ? t('system.mirror.progressUnknown', { done: formatBytes(p.done, lang) })
              : t('system.mirror.progress', {
                  done: formatBytes(p.done, lang),
                  total: formatBytes(p.total, lang),
                  percent: formatPercent(p.fraction, lang),
                })}
          </span>
        </div>
      ) : null}
      {look.facts.length ? (
        <FactList layout="flow" rows={look.facts.map((f) => [t(f.key), show(f)])} />
      ) : null}
      {mirror.schema_newer ? (
        <Notice tone="warn" icon={TriangleAlert} title={t('system.mirror.newer.title')}>
          {t('system.mirror.newer.body')}
        </Notice>
      ) : null}
      {look.error ? (
        <Notice
          tone={mirror.state === 'error' ? 'bad' : 'warn'}
          icon={TriangleAlert}
          title={t(look.error.key)}
        >
          <span className="[overflow-wrap:anywhere]">{look.error.text}</span>
        </Notice>
      ) : null}
      {mirror.fallback ? (
        <Notice tone="info" icon={Globe} title={t('system.mirror.fallback.title')}>
          {t('system.mirror.fallback.body', { host: hostOf(baseUrl) })}
        </Notice>
      ) : null}
      <div className="flex justify-end">
        <Button
          variant="outline"
          size="sm"
          onClick={() => void check()}
          disabled={!look.canCheck || checking}
        >
          <RotateCw className={cn(checking && 'animate-spin')} aria-hidden="true" />
          {checking ? t('common.checking') : t('common.checkNow')}
        </Button>
      </div>
    </div>
  );
}
