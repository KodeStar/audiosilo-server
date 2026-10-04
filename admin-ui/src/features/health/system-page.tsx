import { Link } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import {
  Cpu,
  Database,
  Globe,
  HardDrive,
  MonitorSmartphone,
  Package,
  ShieldCheck,
  Unplug,
  type LucideIcon,
} from 'lucide-react';
import { useSystem } from '@/api/hooks';
import type { SystemStatus } from '@/api/types';
import { Notice } from '@/components/notice';
import { Page } from '@/components/page';
import { PageHead } from '@/components/page-head';
import { QueryError } from '@/components/query-error';
import { buttonVariants } from '@/components/ui/button';
import { formatBytes, formatNumber } from '@/lib/format';
import { cn } from '@/lib/utils';
import { systemRows, type RowStatus, type SystemRow } from './system-model';

const ICONS: Record<SystemRow['kind'], LucideIcon> = {
  ffmpeg: Cpu,
  ffprobe: Cpu,
  metadata: Globe,
  tls: ShieldCheck,
  database: Database,
  library: HardDrive,
  player: MonitorSmartphone,
  update: Package,
};

const STATUS_CLASS: Record<RowStatus, string> = {
  ok: 'text-success',
  warn: 'text-warning',
  bad: 'text-destructive',
  off: 'text-muted-foreground',
};

/**
 * Health > System: everything the server depends on in one place (tools, the
 * community metadata service, the certificate, the database, each library's
 * disk, the web player, updates), each with a plain status. Polled every 30 s.
 */
export function SystemPage() {
  const { t } = useTranslation();
  const system = useSystem();
  const sys = system.data;
  return (
    <Page>
      <PageHead
        title={t('system.title')}
        description={t('system.description', { name: sys?.name ?? 'AudioSilo' })}
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
  const metaDown = sys.metadata.health && !sys.metadata.health.reachable;

  const values = (v?: Record<string, string | number>) =>
    v &&
    Object.fromEntries(
      Object.entries(v).map(([k, x]) => [
        k,
        typeof x !== 'number' || k === 'count'
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
                <span
                  className={cn(
                    'inline-flex min-w-[124px] items-center justify-end gap-1.5 text-[13px] font-semibold max-md:min-w-0',
                    STATUS_CLASS[r.status],
                  )}
                >
                  <span
                    className="dot"
                    data-tone={r.status === 'ok' ? undefined : r.status}
                    aria-hidden="true"
                  />
                  {t(r.statusKey)}
                </span>
              </li>
            );
          })}
        </ul>
      </section>
    </div>
  );
}
