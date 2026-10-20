import { useId, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { ArrowRight, FilePenLine, SearchX } from 'lucide-react';
import { useMatchCandidates, useServerInfo } from '@/api/hooks';
import type { AdminBookDetail, BridgeJob, CorrectionField } from '@/api/types';
import { Notice } from '@/components/notice';
import { QueryError } from '@/components/query-error';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Checkbox } from '@/components/ui/checkbox';
import { Dialog, DialogBody, DialogContent, DialogFormFooter } from '@/components/ui/dialog';
import { Field } from '@/components/ui/field';
import { Textarea } from '@/components/ui/textarea';
import { formatNumber } from '@/lib/format';
import { cn } from '@/lib/utils';
import { queueJob } from '@/features/bridge/bridge-actions';
import { isOpen } from '@/features/bridge/bridge-model';
import { generateJobsKnown, KIND_CORRECT } from '@/features/bridge/generate-model';
import { JobActions, JobSection } from '@/features/bridge/generate-parts';
import { GenerateStatus } from '@/features/bridge/generate-status';
import { useBookGenerateJob, useBridgeLink } from '@/features/bridge/use-generate';
import { shownValue } from './book-model';
import {
  alreadySuggested,
  correctionCandidate,
  correctionProblem,
  correctionRecording,
  correctionRequest,
  correctionRows,
  correctionsOffered,
  defaultCorrectionTicks,
  lookupBy,
  MAX_EVIDENCE,
  MAX_VALUE,
  REFUSALS,
  sendable,
  type CorrectionRow,
} from './corrections-model';

/**
 * The Community card's corrections part (BRIDGE-PLAN.md Phase 2b, decision 18):
 * "Suggest corrections" opens the review of the admin's edits against the
 * community's values, and the book's newest `meta.correct` job shows here
 * while it works and, once filed, with its issue. Offered only while the
 * contributions module is on and the rest of correctionsOffered holds. A
 * corrections job that exists shows with its Cancel whatever the flag says now
 * (the flag is also off while the Bridge is away, and a job exists only if the
 * module was on), and a review already open stays (what was typed isn't lost;
 * sending says why it can't). Renders nothing when there is nothing to show.
 */
export function CorrectionsSection({
  detail,
  blocked,
}: {
  detail: AdminBookDetail;
  /** Unsaved edits on the page: the review reads the saved values, so it waits for them. */
  blocked: boolean;
}) {
  const { t } = useTranslation();
  const server = useServerInfo().data;
  const b = detail.book;
  const { job } = useBookGenerateJob(b.library_id, b.path, generateJobsKnown(server), KIND_CORRECT);
  const uiUrl = useBridgeLink(job?.state === 'needs_attention');
  const [open, setOpen] = useState(false);

  // Not while one is queued or working: the server would hand that one back
  // and ignore new changes, so its status shows instead.
  const offer = correctionsOffered(server, detail) && !(job && isOpen(job.state));
  const shown = job && job.state !== 'cancelled' ? job : undefined;
  if (!open && !offer && !shown) return null;

  return (
    <JobSection id="corrections-title" title={t('book.corrections.title')} job={shown}>
      {offer ? (
        <p className="text-[12.5px] text-muted-foreground">{t('book.corrections.pitch')}</p>
      ) : null}
      {shown ? <GenerateStatus job={shown} uiUrl={uiUrl} /> : null}
      <JobActions job={shown} cancelAria={t('book.corrections.cancelAria')}>
        {offer ? (
          <Button variant="outline" size="sm" disabled={blocked} onClick={() => setOpen(true)}>
            <FilePenLine aria-hidden="true" />
            {t('book.corrections.action')}
          </Button>
        ) : null}
      </JobActions>
      <CorrectionsDialog open={open} onOpenChange={setOpen} detail={detail} lastJob={job} />
    </JobSection>
  );
}

/**
 * The review (CORRECTIONS-SPIKE section 4.1): each edited field that differs
 * from the community's value, ticked unless it can't be sent or was suggested
 * already, with who applies it on the community's side; the evidence (required,
 * prefilled with where the values come from); then one `meta.correct` job,
 * which the Bridge files as one issue.
 */
export function CorrectionsDialog({
  open,
  onOpenChange,
  detail,
  lastJob,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  detail: AdminBookDetail;
  /** The book's newest meta.correct job: what it suggested starts unticked. */
  lastJob?: BridgeJob;
}) {
  const { t } = useTranslation();
  const name = detail.book.title || detail.book.path;
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        title={t('book.corrections.dialog')}
        // Two lines at most, so a long title never squeezes the review on a
        // phone; the whole of it stays in the text (read out) and on hover.
        description={
          <span className="line-clamp-2 [overflow-wrap:anywhere]" title={name}>
            {name}
          </span>
        }
        icon={FilePenLine}
        tone="community"
        size="lg"
      >
        {open ? (
          <CorrectionsForm detail={detail} lastJob={lastJob} onDone={() => onOpenChange(false)} />
        ) : null}
      </DialogContent>
    </Dialog>
  );
}

function CorrectionsForm({
  detail,
  lastJob,
  onDone,
}: {
  detail: AdminBookDetail;
  lastJob?: BridgeJob;
  onDone: () => void;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const qc = useQueryClient();
  const server = useServerInfo().data;
  const b = detail.book;
  const search = useMatchCandidates(b.library_id, b.path, lookupBy(detail.fields), true);
  const candidate = correctionCandidate(search.data);
  const recording = candidate ? correctionRecording(candidate, detail.fields) : undefined;
  const rows = candidate && recording ? correctionRows(detail.fields, candidate, recording) : [];
  const suggested = alreadySuggested(lastJob, detail.fields);
  // The ticks start from the rows once the lookup answers (null until the admin changes one).
  const [ticks, setTicks] = useState<Set<CorrectionField> | null>(null);
  const ticked = ticks ?? defaultCorrectionTicks(rows, suggested);
  const [evidence, setEvidence] = useState(() =>
    t('book.corrections.evidencePrefill', { version: server?.version ?? '' }),
  );
  const [touched, setTouched] = useState(false);
  const [busy, setBusy] = useState(false);
  const evidenceId = useId();
  const sending = rows.filter((r) => ticked.has(r.field) && sendable(r)).length;
  const problem = correctionProblem(sending, evidence);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setTouched(true);
    if (!candidate || !recording || problem || busy) return;
    setBusy(true);
    const ok = await queueJob(
      qc,
      correctionRequest(detail, candidate, recording, rows, ticked, evidence),
      { name: b.title || b.path, online: Boolean(server?.capabilities.bridge) },
    );
    setBusy(false);
    if (ok) onDone();
  };

  // Each evidence problem but noEvidence (said once touched) is its own key.
  const evidenceError =
    problem === 'noEvidence'
      ? touched
        ? t('book.corrections.evidenceRequired')
        : undefined
      : problem && problem !== 'none'
        ? t(`book.corrections.${problem}`, { max: formatNumber(MAX_EVIDENCE, lang) })
        : undefined;

  let body: React.ReactNode;
  if (search.isError) {
    body = (
      <QueryError
        title={t('book.corrections.lookupError')}
        error={search.error}
        onRetry={() => void search.refetch()}
      />
    );
  } else if (search.isPending) {
    body = <div className="skel h-40 rounded-lg" role="status" aria-label={t('common.loading')} />;
  } else if (!candidate) {
    body = (
      <Notice tone="warn" icon={SearchX} title={t('book.corrections.notFound')}>
        {t('book.corrections.notFoundBody')}
      </Notice>
    );
  } else if (!rows.length) {
    body = <p className="text-muted-foreground">{t('book.corrections.agrees')}</p>;
  } else {
    body = (
      <>
        <p className="text-[13px] text-muted-foreground">
          {t('book.corrections.intro', { title: candidate.title })}
        </p>
        <ul
          className="flex flex-col divide-y rounded-[12px] border"
          aria-label={t('book.corrections.rows')}
        >
          {rows.map((r) => (
            <ReviewRow
              key={r.field}
              row={r}
              on={ticked.has(r.field)}
              suggested={suggested.has(r.field)}
              onToggle={(on) => {
                const next = new Set(ticked);
                if (on) next.add(r.field);
                else next.delete(r.field);
                setTicks(next);
              }}
            />
          ))}
        </ul>
        <Field
          htmlFor={evidenceId}
          label={t('book.corrections.evidence')}
          description={t('book.corrections.evidenceHint')}
          error={evidenceError}
        >
          <Textarea
            id={evidenceId}
            value={evidence}
            required
            aria-invalid={evidenceError ? true : undefined}
            aria-describedby={evidenceError ? `${evidenceId}-error` : `${evidenceId}-desc`}
            onChange={(e) => setEvidence(e.target.value)}
          />
        </Field>
        <p className="text-[12.5px] text-muted-foreground">{t('book.corrections.where')}</p>
      </>
    );
  }

  return (
    <form onSubmit={(e) => void submit(e)} className="contents" noValidate>
      <DialogBody className="flex flex-col gap-4">{body}</DialogBody>
      <DialogFormFooter
        busy={busy}
        disabled={!candidate || problem === 'none'}
        submitLabel={t('book.corrections.submit', {
          count: sending,
          n: formatNumber(sending, lang),
        })}
      />
    </form>
  );
}

/** One field of the review: its tick, the community's value and the admin's, and who applies it. */
function ReviewRow({
  row: r,
  on,
  suggested,
  onToggle,
}: {
  row: CorrectionRow;
  on: boolean;
  suggested: boolean;
  onToggle: (on: boolean) => void;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const id = useId();
  const label = t(`book.field.${r.field}`);
  const ok = sendable(r);
  const flag = REFUSALS.find((k) => r[k]);
  const refusal = flag && t(`book.corrections.${flag}`, { max: formatNumber(MAX_VALUE, lang) });
  return (
    <li className="flex items-start gap-3 px-4 py-3">
      <Checkbox
        id={id}
        className="mt-0.5"
        checked={on && ok}
        disabled={!ok}
        onCheckedChange={(v) => onToggle(v)}
        aria-label={t('book.corrections.tickAria', { field: label })}
      />
      <div className="flex min-w-0 flex-1 flex-col gap-1.5">
        <div className="flex flex-wrap items-center gap-2">
          <label htmlFor={id} className="cursor-pointer font-semibold">
            {label}
          </label>
          <Badge variant={r.handling === 'auto' ? 'success' : 'outline'}>
            {r.handling === 'auto' ? t('book.corrections.auto') : t('book.corrections.maintainer')}
          </Badge>
          {suggested ? (
            <Badge variant="secondary">{t('book.corrections.suggestedBefore')}</Badge>
          ) : null}
        </div>
        <div className="flex flex-wrap items-baseline gap-x-2 gap-y-1 text-[13px]">
          <span
            className={cn(
              '[overflow-wrap:anywhere] text-muted-foreground',
              on && ok && 'line-through',
            )}
          >
            <span className="sr-only">{t('book.corrections.communityNow')} </span>
            {r.current ? (
              shownValue(t, r.field, r.current)
            ) : (
              <i className="text-subtle-foreground">{t('book.match.notProvided')}</i>
            )}
          </span>
          <ArrowRight className="size-3.5 shrink-0 text-subtle-foreground" aria-hidden="true" />
          <span className="font-[550] [overflow-wrap:anywhere]">
            <span className="sr-only">{t('book.corrections.yours')} </span>
            {shownValue(t, r.field, r.corrected)}
          </span>
        </div>
        {refusal ? <p className="text-[12.5px] text-warning">{refusal}</p> : null}
      </div>
    </li>
  );
}
