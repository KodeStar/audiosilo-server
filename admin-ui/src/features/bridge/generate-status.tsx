import { useTranslation } from 'react-i18next';
import { CircleCheck, ExternalLink, TriangleAlert } from 'lucide-react';
import type { BridgeJob } from '@/api/types';
import { ProgressBar } from '@/components/progress-bar';
import { formatDuration, formatList, formatNumber, formatRelative, formatUsd } from '@/lib/format';
import {
  bridgeJobLink,
  generateProgress,
  generateResult,
  KIND_DESCRIBE,
  parkKey,
  roundEta,
  stageLabel,
} from './generate-model';
import { isOpen } from './bridge-model';

/**
 * Where a `sidecars.generate` (or `sidecars.describe`) job stands, in a few
 * lines: while it works, the stage, the count, the ETA and the cost so far (with
 * a bar when there is a count); parked, why and the hint, with a link into the
 * Bridge's own page; done, what it made (a generate job's description outcome
 * too); failed, the error. The book page's Community card and the Bridge page's
 * job rows both show a job this way.
 */
export function GenerateStatus({ job, uiUrl }: { job: BridgeJob; uiUrl?: string }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const p = generateProgress(job.progress);

  if (job.state === 'done') {
    const r = generateResult(job.result);
    const describe = job.kind === KIND_DESCRIBE;
    const made = describe
      ? [t('bridge.describe.done')]
      : [
          r?.characters !== undefined
            ? t('bridge.generate.characters', {
                count: r.characters,
                n: formatNumber(r.characters, lang),
              })
            : undefined,
          r?.recaps !== undefined
            ? t('bridge.generate.recaps', { count: r.recaps, n: formatNumber(r.recaps, lang) })
            : undefined,
          r?.description ? t('bridge.generate.descriptionMade') : undefined,
        ].filter((s): s is string => Boolean(s));
    const facts = [
      made.length ? formatList(made, lang) : t('bridge.generate.doneNoCounts'),
      r?.costUsd !== undefined
        ? t('bridge.generate.costTotal', { cost: formatUsd(r.costUsd, lang) })
        : undefined,
      job.finished_at
        ? t('bridge.generate.finished', { when: formatRelative(job.finished_at, lang) })
        : undefined,
    ].filter(Boolean);
    return (
      <div className="flex flex-col gap-1">
        <p className="flex items-start gap-1.5 text-[12.5px] text-success">
          <CircleCheck className="mt-px size-[15px] shrink-0" aria-hidden="true" />
          <span className="text-foreground">{facts.join(' · ')}</span>
        </p>
        {!describe && r?.description === false ? (
          <p className="text-[12.5px] text-muted-foreground [overflow-wrap:anywhere]">
            {r.descriptionError
              ? t('bridge.generate.noDescription', { reason: r.descriptionError })
              : t('bridge.generate.noDescriptionPlain')}
          </p>
        ) : null}
        {r?.contributed.map((c) => (
          <a
            key={c.what}
            href={c.url}
            target="_blank"
            rel="noopener noreferrer"
            className="inline-flex items-center gap-1 text-[12.5px] font-semibold text-brand-ink hover:underline"
          >
            {t('bridge.generate.contributed', {
              what: t(`bridge.generate.offered.${c.what}`, { defaultValue: c.what }),
            })}
            <ExternalLink className="size-3.5" aria-hidden="true" />
          </a>
        ))}
      </div>
    );
  }

  if (job.state === 'failed') {
    return job.error ? (
      <p className="text-[12.5px] text-destructive [overflow-wrap:anywhere]">{job.error}</p>
    ) : null;
  }

  // Cancelled: why, when the server or the Bridge said (a book moved onto
  // another job of its kind, say); an admin's own cancel says nothing more.
  if (job.state === 'cancelled') {
    return job.error ? (
      <p className="text-[12.5px] text-muted-foreground [overflow-wrap:anywhere]">{job.error}</p>
    ) : null;
  }

  if (job.state === 'needs_attention') {
    const link = bridgeJobLink(uiUrl, job.id);
    const park = p.park;
    // The hint says what to do; without one, the park message (the job's error) does.
    const hint = park?.hint ?? (job.error || undefined);
    return (
      <div className="flex flex-col gap-1.5 rounded-lg border border-[color-mix(in_oklab,var(--warning)_30%,var(--border))] bg-warning-soft/40 px-3 py-2.5">
        <p className="flex items-start gap-1.5 text-[12.5px] font-semibold">
          <TriangleAlert className="mt-px size-[15px] shrink-0 text-warning" aria-hidden="true" />
          <span>
            {park ? t(parkKey(park.code), { code: park.code }) : t('bridge.state.needs_attention')}
          </span>
        </p>
        {hint ? (
          <p className="text-[12.5px] text-muted-foreground [overflow-wrap:anywhere]">{hint}</p>
        ) : null}
        {park?.retryAt ? (
          <p className="text-[12.5px] text-muted-foreground">
            {/* Once the time has passed the Bridge is retrying: never "tries again ... ago". */}
            {Date.parse(park.retryAt) > Date.now()
              ? t('bridge.generate.retryAt', { when: formatRelative(park.retryAt, lang) })
              : t('bridge.generate.retrying')}
          </p>
        ) : null}
        {link ? (
          <a
            href={link}
            target="_blank"
            rel="noopener noreferrer"
            className="inline-flex items-center gap-1 self-start text-[12.5px] font-semibold text-brand-ink hover:underline"
          >
            {t('bridge.generate.openInBridge')}
            <ExternalLink className="size-3.5" aria-hidden="true" />
          </a>
        ) : null}
      </div>
    );
  }

  if (!isOpen(job.state)) return null;

  // Queued: nothing has started; say so rather than a stage.
  const label = p.stage ? stageLabel(p.stage) : undefined;
  const stage =
    job.state === 'queued'
      ? t('bridge.generate.waiting')
      : label
        ? t(label.key, { defaultValue: label.fallback })
        : t('bridge.generate.starting');
  const facts = [
    stage,
    p.total
      ? t('bridge.jobs.count', {
          done: formatNumber(p.done ?? 0, lang),
          total: formatNumber(p.total, lang),
        })
      : undefined,
    p.etaSeconds
      ? t('bridge.generate.eta', { time: formatDuration(roundEta(p.etaSeconds), lang) })
      : undefined,
    p.costUsd !== undefined
      ? t('bridge.generate.costSoFar', { cost: formatUsd(p.costUsd, lang) })
      : undefined,
  ].filter(Boolean);
  return (
    <div className="flex flex-col gap-1.5">
      <p className="text-[12.5px] text-muted-foreground tabular-nums" aria-live="polite">
        <span className="font-semibold text-foreground">{facts[0]}</span>
        {facts.length > 1 ? ` · ${facts.slice(1).join(' · ')}` : null}
      </p>
      {p.message && p.message !== p.stage ? (
        <p className="text-[12px] text-subtle-foreground [overflow-wrap:anywhere]">{p.message}</p>
      ) : null}
      {p.total ? <ProgressBar fraction={(p.done ?? 0) / p.total} /> : null}
    </div>
  );
}
