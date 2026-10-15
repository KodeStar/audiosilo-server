import { useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { ChevronRight, ListChecks, X } from 'lucide-react';
import type { BridgeJob } from '@/api/types';
import { QueryError } from '@/components/query-error';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Card, CardHeader } from '@/components/ui/card';
import { NativeSelect } from '@/components/ui/native-select';
import { formatDateTime, formatNumber, formatRelative, formatTook } from '@/lib/format';
import { CardEmpty } from '@/features/settings/card-empty';
import { cancelJob } from './bridge-actions';
import {
  BRIDGE_JOB_STATES,
  hasContent,
  jobActions,
  kindKey,
  progressOf,
  resultSummary,
  stateLook,
} from './bridge-model';
import { useBridgeJobs } from './use-bridge';

/**
 * The Bridge's jobs, newest first, a page at a time, maybe in one state: what
 * each is about, where it stands, who asked, and Cancel for an open one.
 * `hideWhenEmpty` (no Bridge paired) renders nothing until there is a job.
 */
export function JobsCard({
  libraryNames,
  hideWhenEmpty = false,
}: {
  libraryNames: ReadonlyMap<number, string>;
  hideWhenEmpty?: boolean;
}) {
  const { t } = useTranslation();
  const [state, setState] = useState('');
  const jobs = useBridgeJobs(state);
  const rows = jobs.data?.pages.flatMap((p) => p.jobs) ?? [];
  // Unfiltered and empty (or not loaded yet): no card. A filter that matches
  // nothing keeps the card, so it can be cleared.
  if (hideWhenEmpty && state === '' && !jobs.isError && rows.length === 0) return null;

  return (
    <Card aria-labelledby="bridge-jobs-title">
      <CardHeader
        titleId="bridge-jobs-title"
        title={t('bridge.jobs.title')}
        description={t('bridge.jobs.description')}
        action={
          <NativeSelect
            className="w-[190px]"
            value={state}
            onChange={(e) => setState(e.target.value)}
            aria-label={t('bridge.jobs.filter')}
          >
            <option value="">{t('bridge.jobs.allStates')}</option>
            {BRIDGE_JOB_STATES.map((s) => (
              <option key={s} value={s}>
                {t(`bridge.state.${s}`)}
              </option>
            ))}
          </NativeSelect>
        }
      />
      {jobs.isError ? (
        <div className="p-5">
          <QueryError
            title={t('bridge.jobs.error')}
            error={jobs.error}
            onRetry={() => void jobs.refetch()}
          />
        </div>
      ) : jobs.isPending ? (
        <div className="p-5">
          <div className="skel h-24 rounded-lg" role="status" aria-label={t('common.loading')} />
        </div>
      ) : rows.length === 0 ? (
        <CardEmpty
          icon={ListChecks}
          title={state ? t('bridge.jobs.noneInState') : t('bridge.jobs.none')}
          body={t('bridge.jobs.noneBody')}
        />
      ) : (
        <ul className="flex flex-col divide-y" aria-label={t('bridge.jobs.title')}>
          {rows.map((j) => (
            <JobRow key={j.id} job={j} libraryNames={libraryNames} />
          ))}
        </ul>
      )}
      {jobs.hasNextPage ? (
        <div className="flex justify-center border-t px-5 py-3">
          <Button
            variant="outline"
            size="sm"
            onClick={() => void jobs.fetchNextPage()}
            disabled={jobs.isFetchingNextPage}
          >
            {jobs.isFetchingNextPage ? t('common.loading') : t('bridge.jobs.older')}
          </Button>
        </div>
      ) : null}
    </Card>
  );
}

function JobRow({
  job,
  libraryNames,
}: {
  job: BridgeJob;
  libraryNames: ReadonlyMap<number, string>;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const qc = useQueryClient();
  const [open, setOpen] = useState(false);
  const [cancelling, setCancelling] = useState(false);
  const actions = jobActions(job);
  const look = stateLook(job);
  const key = kindKey(job.kind);
  const kind = key ? t(key) : job.kind;
  const library =
    job.library_id === null
      ? ''
      : (libraryNames.get(job.library_id) ??
        t('bridge.jobs.libraryId', { id: formatNumber(job.library_id, lang) }));
  const progress = progressOf(job.progress);
  const summary = resultSummary(job.result);
  const details = hasContent(job.result) || hasContent(job.params);

  const meta = [
    job.requested_by
      ? t('bridge.jobs.by', { name: job.requested_by.username })
      : t('bridge.jobs.byNobody'),
    t('bridge.jobs.created', { when: formatRelative(job.created_at, lang) }),
  ];
  if (job.updated_at !== job.created_at)
    meta.push(t('bridge.jobs.updated', { when: formatRelative(job.updated_at, lang) }));
  if (job.attempts > 1)
    meta.push(t('bridge.jobs.attempt', { n: formatNumber(job.attempts, lang) }));

  const cancel = async () => {
    setCancelling(true);
    await cancelJob(qc, job);
    setCancelling(false);
  };

  return (
    <li className="flex flex-col gap-2 px-5 py-3.5">
      <div className="flex flex-wrap items-start gap-x-3 gap-y-2">
        <div className="flex min-w-0 flex-1 basis-64 flex-col gap-0.5">
          <span className="flex flex-wrap items-center gap-2">
            <b className={key ? 'font-semibold' : 'font-mono text-[12.5px] font-semibold'}>
              {kind}
            </b>
            <Badge variant={look.tone}>{t(look.key)}</Badge>
          </span>
          {library ? (
            <span className="text-[12.5px] [overflow-wrap:anywhere]">
              {library}
              {job.path ? (
                <>
                  {' · '}
                  <span className="font-mono text-[12px]">{job.path}</span>
                </>
              ) : (
                <span className="text-muted-foreground"> · {t('bridge.jobs.wholeLibrary')}</span>
              )}
            </span>
          ) : null}
          {progress.text || progress.total ? (
            <span className="text-[12.5px] text-muted-foreground">
              {[
                progress.text,
                progress.total
                  ? t('bridge.jobs.count', {
                      done: formatNumber(progress.done ?? 0, lang),
                      total: formatNumber(progress.total, lang),
                    })
                  : undefined,
              ]
                .filter(Boolean)
                .join(' · ')}
            </span>
          ) : null}
          <span
            className="text-[12px] text-subtle-foreground"
            title={formatDateTime(job.created_at, lang, true)}
          >
            {meta.join(' · ')}
          </span>
        </div>
        <div className="flex items-center gap-1.5">
          {actions.cancel ? (
            <Button
              variant="ghost"
              size="sm"
              disabled={cancelling}
              onClick={() => void cancel()}
              aria-label={t('bridge.jobs.cancelAria', { kind, id: job.id })}
            >
              <X aria-hidden="true" />
              <span className="max-md:sr-only">{t('bridge.jobs.cancel')}</span>
            </Button>
          ) : null}
          {details ? (
            <Button variant="ghost" size="sm" aria-expanded={open} onClick={() => setOpen(!open)}>
              <ChevronRight
                className={open ? 'rotate-90 transition-transform' : 'transition-transform'}
                aria-hidden="true"
              />
              {t('bridge.jobs.details')}
            </Button>
          ) : null}
        </div>
      </div>
      {job.error ? (
        <p className="text-[12.5px] text-destructive [overflow-wrap:anywhere]">{job.error}</p>
      ) : null}
      {summary ? <ResultLine summary={summary} lang={lang} /> : null}
      {open ? (
        <div className="flex flex-col gap-2">
          {hasContent(job.result) ? (
            <Json label={t('bridge.jobs.result')} value={job.result} />
          ) : null}
          {hasContent(job.params) ? (
            <Json label={t('bridge.jobs.params')} value={job.params} />
          ) : null}
        </div>
      ) : null}
    </li>
  );
}

/** A result's facts in a line ("4 files checked, all match · Can write · Took 1.2 s"). */
function ResultLine({
  summary,
  lang,
}: {
  summary: NonNullable<ReturnType<typeof resultSummary>>;
  lang: string;
}) {
  const { t } = useTranslation();
  const parts: string[] = [];
  if (summary.files) {
    const { checked, ok } = summary.files;
    parts.push(
      ok === checked
        ? t('bridge.result.filesOk', { count: checked, n: formatNumber(checked, lang) })
        : t('bridge.result.filesBad', {
            count: checked,
            n: formatNumber(checked, lang),
            bad: formatNumber(checked - ok, lang),
          }),
    );
  }
  if (summary.writable !== undefined)
    parts.push(summary.writable ? t('bridge.result.writable') : t('bridge.result.readOnly'));
  if (summary.elapsedMs !== undefined)
    parts.push(t('bridge.result.took', { time: formatTook(summary.elapsedMs / 1000, lang) }));
  return (
    <p className="text-[12.5px] text-muted-foreground [overflow-wrap:anywhere]">
      {parts.join(' · ')}
      {summary.mountPath ? (
        <>
          {parts.length ? ' · ' : ''}
          <span className="font-mono text-[12px]">{summary.mountPath}</span>
        </>
      ) : null}
    </p>
  );
}

/** A raw JSON block, for any kind's result or params. */
function Json({ label, value }: { label: string; value: unknown }) {
  return (
    <div className="flex flex-col gap-1">
      <span className="text-[12px] font-semibold text-muted-foreground">{label}</span>
      <pre className="max-h-[320px] overflow-auto rounded-lg bg-muted px-3 py-2.5 font-mono text-[12px] leading-5">
        {JSON.stringify(value, null, 2)}
      </pre>
    </div>
  );
}
