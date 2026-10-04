import { useState } from 'react';
import { Link } from '@tanstack/react-router';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { CalendarClock, Check, ChevronDown, Play, RefreshCw, Square, X } from 'lucide-react';
import { api } from '@/api/client';
import { keys, useJobs, useLibraries, useScanRun, useScanRuns } from '@/api/hooks';
import type { Job, ScanRun, ScheduledScan } from '@/api/types';
import { Page } from '@/components/page';
import { PageHead } from '@/components/page-head';
import { Notice } from '@/components/notice';
import { QueryError } from '@/components/query-error';
import { Badge } from '@/components/ui/badge';
import { Button, buttonVariants } from '@/components/ui/button';
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu';
import { NativeSelect } from '@/components/ui/native-select';
import { rescanAll, rescanLibrary } from '@/features/libraries/rescan';
import { ScanProgressBar } from '@/features/libraries/library-card';
import { scheduleLabel } from '@/features/libraries/scan-settings';
import { toastError } from '@/lib/errors';
import {
  formatClockTime,
  formatDateTime,
  formatNumber,
  formatRelative,
  formatTook,
} from '@/lib/format';
import { toast } from '@/lib/toast';
import { cn } from '@/lib/utils';
import { say } from './issues-model';
import { eventPhrase, runSeconds, runSummary, statusTone } from './jobs-model';

/**
 * Health > Jobs: the scan running now (live), what waits behind it, the
 * schedules, and every recorded scan with its log. One job runs at a time.
 */
export function JobsPage() {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const libraries = useLibraries();
  const jobs = useJobs();
  const libs = libraries.data ?? [];

  const runMenu = (
    <DropdownMenu>
      <DropdownMenuTrigger className={buttonVariants()} disabled={!libs.length}>
        <Play aria-hidden="true" />
        {t('jobs.run')}
        <ChevronDown aria-hidden="true" />
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end">
        {libs.map((l) => (
          <DropdownMenuItem key={l.id} onClick={() => rescanLibrary(qc, l)}>
            <RefreshCw aria-hidden="true" />
            {t('jobs.rescanLibrary', { name: l.name })}
          </DropdownMenuItem>
        ))}
        {libs.length > 1 ? (
          <>
            <DropdownMenuSeparator />
            <DropdownMenuItem onClick={() => rescanAll(qc, libs)}>
              <RefreshCw aria-hidden="true" />
              {t('jobs.rescanAll')}
            </DropdownMenuItem>
          </>
        ) : null}
      </DropdownMenuContent>
    </DropdownMenu>
  );

  const cancel = async (job: Job) => {
    try {
      await api.cancelJob(job.id);
      toast.add({
        title: job.started_at
          ? t('jobs.toast.stopped', { name: job.library_name })
          : t('jobs.toast.dropped', { name: job.library_name }),
        description: job.started_at ? t('jobs.toast.stoppedBody') : undefined,
        type: 'success',
      });
    } catch (err) {
      toastError(t('jobs.toast.cancelFailed'), err);
    } finally {
      void qc.invalidateQueries({ queryKey: keys.jobs });
      void qc.invalidateQueries({ queryKey: keys.libraries });
    }
  };

  return (
    <Page>
      <PageHead title={t('jobs.title')} description={t('jobs.description')} action={runMenu} />
      {jobs.isError ? (
        <QueryError
          title={t('jobs.error')}
          error={jobs.error}
          onRetry={() => void jobs.refetch()}
        />
      ) : (
        <div className="grid items-start gap-4 xl:grid-cols-[minmax(0,1fr)_360px]">
          <div className="flex min-w-0 flex-col gap-4">
            <NowCard job={jobs.data?.running} loading={jobs.isPending} onStop={cancel} />
            {jobs.data?.queued.length ? (
              <QueueCard jobs={jobs.data.queued} onCancel={cancel} />
            ) : null}
          </div>
          <SchedulesCard schedules={jobs.data?.schedules} />
        </div>
      )}
      <History />
    </Page>
  );
}

/** The live readout of the running scan, or "nothing running". */
function NowCard({
  job,
  loading,
  onStop,
}: {
  job: Job | null | undefined;
  loading: boolean;
  onStop: (job: Job) => void;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  if (loading) return <div className="skel h-[148px] rounded-xl" />;
  if (!job) {
    return (
      <section aria-label={t('jobs.now')}>
        <Notice tone="safe" icon={Check} title={t('jobs.idle.title')}>
          {t('jobs.idle.body')}
        </Notice>
      </section>
    );
  }
  const p = job.progress;
  const counters: [string, number][] = p
    ? [
        [t('jobs.counter.checked'), p.done],
        [t('jobs.counter.added'), p.added],
        [t('jobs.counter.updated'), p.updated],
        [t('jobs.counter.moved'), p.moved],
        [t('jobs.counter.removed'), p.removed],
      ]
    : [];
  return (
    <section
      className="flex flex-col gap-4 rounded-xl border bg-card p-5"
      aria-label={t('jobs.now')}
    >
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex min-w-0 items-center gap-3">
          <span
            className="grid size-[38px] shrink-0 place-items-center rounded-[11px] bg-brand-soft text-brand-ink"
            aria-hidden="true"
          >
            <RefreshCw className="size-[18px] animate-spin motion-reduce:animate-none" />
          </span>
          <div className="flex min-w-0 flex-col">
            <b className="truncate">{t('jobs.scanning', { name: job.library_name })}</b>
            <span className="text-[12.5px] text-muted-foreground">
              {t(`jobs.trigger.${job.trigger}`)}
              {job.started_at
                ? ` · ${t('jobs.startedAgo', { when: formatRelative(job.started_at, lang) })}`
                : ''}
            </span>
          </div>
        </div>
        <div className="flex items-center gap-2">
          <Badge variant="brand">{t('jobs.running')}</Badge>
          <Button variant="outline" size="sm" onClick={() => onStop(job)}>
            <Square aria-hidden="true" />
            {t('jobs.stop')}
          </Button>
        </div>
      </div>
      {p ? <ScanProgressBar progress={p} lang={lang} /> : null}
      <dl className="grid grid-cols-3 gap-2 sm:grid-cols-5">
        {counters.map(([label, n]) => (
          <div key={label} className="flex flex-col-reverse">
            <dt className="text-[12px] text-muted-foreground">{label}</dt>
            <dd className="font-display text-[22px] font-bold tabular-nums">
              {formatNumber(n, lang)}
            </dd>
          </div>
        ))}
      </dl>
    </section>
  );
}

/** Scans waiting their turn, each with Cancel. */
function QueueCard({ jobs, onCancel }: { jobs: Job[]; onCancel: (job: Job) => void }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  return (
    <section className="rounded-xl border bg-card" aria-labelledby="queue-heading">
      <h2 id="queue-heading" className="border-b px-5 py-3.5 text-[14.5px] font-semibold">
        {t('jobs.queue', { count: jobs.length })}
      </h2>
      <ol className="divide-y">
        {jobs.map((j) => (
          <li key={j.id} className="flex items-center gap-3 px-5 py-3">
            <div className="flex min-w-0 flex-1 flex-col">
              <span className="truncate font-semibold">
                {t('jobs.scanOf', { name: j.library_name })}
              </span>
              <span className="text-[12.5px] text-muted-foreground">
                {t(`jobs.trigger.${j.trigger}`)} ·{' '}
                {t('jobs.queuedAgo', { when: formatRelative(j.queued_at, lang) })}
              </span>
            </div>
            <Button
              variant="ghost"
              size="sm"
              onClick={() => onCancel(j)}
              aria-label={t('jobs.cancelAria', { name: j.library_name })}
            >
              <X aria-hidden="true" />
              <span className="max-md:sr-only">{t('jobs.cancel')}</span>
            </Button>
          </li>
        ))}
      </ol>
    </section>
  );
}

/** Each scheduled library's next scan; schedules are set when editing a library. */
function SchedulesCard({ schedules }: { schedules: ScheduledScan[] | undefined }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  return (
    <section className="rounded-xl border bg-card" aria-labelledby="schedules-heading">
      <div className="flex items-center justify-between gap-3 border-b px-5 py-3.5">
        <h2 id="schedules-heading" className="text-[14.5px] font-semibold">
          {t('jobs.schedules')}
        </h2>
        <Link
          to="/library/{-$section}"
          params={{ section: 'libraries' }}
          className={buttonVariants({ variant: 'ghost', size: 'sm' })}
        >
          {t('jobs.editSchedules')}
        </Link>
      </div>
      {!schedules ? (
        <div className="p-5">
          <div className="skel h-10" />
        </div>
      ) : schedules.length === 0 ? (
        <p className="px-5 py-4 text-[13px] text-muted-foreground">{t('jobs.noSchedules')}</p>
      ) : (
        <ul className="divide-y">
          {schedules.map((s) => (
            <li key={s.library_id} className="flex items-center gap-3 px-5 py-3">
              <CalendarClock className="size-4 shrink-0 text-muted-foreground" aria-hidden="true" />
              <div className="flex min-w-0 flex-1 flex-col">
                <span className="truncate font-semibold">{s.library_name}</span>
                <span className="text-[12.5px] text-muted-foreground">
                  {scheduleLabel(s.schedule, t)}
                </span>
              </div>
              <span
                className="text-[12.5px] whitespace-nowrap text-muted-foreground"
                title={formatDateTime(s.next_at, lang)}
              >
                {formatRelative(s.next_at, lang)}
              </span>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

/** Every recorded scan, newest first, filterable by library; a row opens its log. */
function History() {
  const { t } = useTranslation();
  const libraries = useLibraries();
  const [libraryId, setLibraryId] = useState(0);
  const runs = useScanRuns(libraryId);
  const [open, setOpen] = useState<number | null>(null);
  const rows = runs.data?.pages.flatMap((p) => p.runs ?? []) ?? [];
  return (
    <section className="mt-10" aria-labelledby="history-heading">
      <div className="mb-3 flex flex-wrap items-end justify-between gap-3">
        <h2 id="history-heading" className="h2">
          {t('jobs.history')}
        </h2>
        <label className="flex items-center gap-2 text-[13px] text-muted-foreground">
          <span className="sr-only">{t('jobs.filterLibrary')}</span>
          <NativeSelect
            className="w-[220px]"
            value={libraryId}
            onChange={(e) => {
              setOpen(null);
              setLibraryId(Number(e.target.value));
            }}
            aria-label={t('jobs.filterLibrary')}
          >
            <option value={0}>{t('jobs.allLibraries')}</option>
            {(libraries.data ?? []).map((l) => (
              <option key={l.id} value={l.id}>
                {l.name}
              </option>
            ))}
          </NativeSelect>
        </label>
      </div>
      {runs.isError ? (
        <QueryError
          title={t('jobs.historyError')}
          error={runs.error}
          onRetry={() => void runs.refetch()}
        />
      ) : runs.isPending ? (
        <div className="skel h-[180px] rounded-xl" role="status" aria-label={t('common.loading')} />
      ) : rows.length === 0 ? (
        <p className="rounded-xl border bg-card px-5 py-6 text-center text-muted-foreground">
          {t('jobs.noHistory')}
        </p>
      ) : (
        <ul className="divide-y rounded-xl border bg-card">
          {rows.map((r) => (
            <RunRow
              key={r.id}
              run={r}
              open={open === r.id}
              onToggle={() => setOpen(open === r.id ? null : r.id)}
            />
          ))}
        </ul>
      )}
      {runs.hasNextPage ? (
        <div className="mt-3 flex justify-center">
          <Button
            variant="outline"
            onClick={() => void runs.fetchNextPage()}
            disabled={runs.isFetchingNextPage}
          >
            {runs.isFetchingNextPage ? t('common.loading') : t('jobs.olderScans')}
          </Button>
        </div>
      ) : null}
    </section>
  );
}

function RunRow({ run: r, open, onToggle }: { run: ScanRun; open: boolean; onToggle: () => void }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const took = runSeconds(r);
  const tone = statusTone(r.status);
  const who = r.started_by_name ? r.started_by_name : t(`jobs.trigger.${r.trigger}`);
  return (
    <li>
      <div className="flex flex-wrap items-center gap-x-4 gap-y-1 px-5 py-3">
        <div className="flex min-w-[180px] flex-1 flex-col">
          <b className="font-semibold">{t('jobs.scanOf', { name: r.library_name })}</b>
          <span className="text-[12.5px] text-muted-foreground">
            {who} ·{' '}
            <span title={formatDateTime(r.started_at, lang)}>
              {formatRelative(r.started_at, lang)}
            </span>
            {took !== undefined ? ` · ${formatTook(took, lang)}` : ''}
          </span>
        </div>
        <span
          className={cn(
            'inline-flex min-w-0 basis-full items-center gap-1.5 text-[13px] md:basis-auto',
            tone === 'bad' && 'text-destructive',
            tone === 'warn' && 'text-warning',
          )}
        >
          <span className="dot" data-tone={tone === 'ok' ? undefined : tone} aria-hidden="true" />
          {runSummary(r)
            .map((p) => say(t, p, lang))
            .join(' · ')}
        </span>
        <Button variant="ghost" size="sm" onClick={onToggle} aria-expanded={open}>
          {open ? t('jobs.hideLog') : t('jobs.showLog')}
        </Button>
      </div>
      {open ? <RunLog id={r.id} /> : null}
    </li>
  );
}

/** A scan's log, fetched when opened. */
function RunLog({ id }: { id: number }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const run = useScanRun(id);
  if (run.isError) {
    return (
      <div className="px-5 pb-4">
        <QueryError
          title={t('jobs.logError')}
          error={run.error}
          onRetry={() => void run.refetch()}
        />
      </div>
    );
  }
  if (!run.data) return <div className="mx-5 mb-4 skel h-24 rounded-lg" />;
  const log = run.data.log ?? [];
  return (
    <div className="px-5 pb-4">
      <ol className="max-h-[360px] overflow-auto rounded-lg bg-muted px-3 py-2.5 font-mono text-[12px] leading-5">
        {log.length === 0 ? <li className="text-muted-foreground">{t('jobs.emptyLog')}</li> : null}
        {log.map((e, i) => {
          const phrase = eventPhrase(e, (code) => t(`health.code.${code}`));
          return (
            <li key={i} className="flex gap-3 [overflow-wrap:anywhere]">
              <span className="shrink-0 text-subtle-foreground tabular-nums">
                {formatClockTime(e.at, lang)}
              </span>
              <span
                className={cn(
                  'w-11 shrink-0 font-semibold uppercase',
                  e.level === 'error'
                    ? 'text-destructive'
                    : e.level === 'warn'
                      ? 'text-warning'
                      : 'text-info',
                )}
              >
                {t(`jobs.level.${e.level}`)}
              </span>
              <span className="min-w-0">{say(t, phrase, lang)}</span>
            </li>
          );
        })}
      </ol>
    </div>
  );
}
