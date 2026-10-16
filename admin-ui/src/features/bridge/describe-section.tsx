import { useId, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { PenLine } from 'lucide-react';
import { useServerInfo } from '@/api/hooks';
import type { AdminBook, GeneratedDescription } from '@/api/types';
import { Button } from '@/components/ui/button';
import { Dialog, DialogBody, DialogContent, DialogFormFooter } from '@/components/ui/dialog';
import { formatRelative } from '@/lib/format';
import { queueJob } from './bridge-actions';
import { jobActions } from './bridge-model';
import {
  clampsDescription,
  describeOffered,
  generateAvailable,
  generateJobsKnown,
  KIND_DESCRIBE,
} from './generate-model';
import { JobCancelButton, JobSection } from './generate-parts';
import { GenerateStatus } from './generate-status';
import { useBookGenerateJob, useBridgeLink, useGenerateEstimate } from './use-generate';

/**
 * The Community card's description part (Bridge Phase 1c): the description the
 * Bridge generated for the book (its text, clamped, with Show more), the
 * describe job while one is open or how it failed, and "Write a description",
 * which queues `sidecars.describe` after a confirmation. Offered only when the
 * Bridge can write one (describeOffered). Renders nothing when there is nothing
 * to show.
 */
export function DescribeSection({
  book,
  description,
}: {
  book: AdminBook;
  description?: GeneratedDescription;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const server = useServerInfo().data;
  const available = generateAvailable(server);
  const { job, jobs } = useBookGenerateJob(
    book.library_id,
    book.path,
    generateJobsKnown(server),
    KIND_DESCRIBE,
  );
  // The same brief estimate the characters-and-recaps part reads (one query).
  const estimate = useGenerateEstimate(book.library_id, book.path, available, { brief: true });
  const parked = job?.state === 'needs_attention';
  const uiUrl = useBridgeLink(parked);
  const [open, setOpen] = useState(false);
  const [expanded, setExpanded] = useState(false);
  const textId = useId();

  const offer = available && describeOffered(estimate.data, jobs);
  // A cancelled job says nothing; a finished one is the description itself once it shows.
  const showJob = job && job.state !== 'cancelled' && !(job.state === 'done' && description);
  if (!open && !showJob && !offer && !description) return null;

  const canCancel = job ? jobActions(job).cancel : false;
  const clamps = description ? clampsDescription(description.text) : false;

  return (
    <JobSection
      id="describe-title"
      title={t('bridge.describe.title')}
      job={showJob ? job : undefined}
    >
      {description ? (
        <>
          <p className="text-[12.5px] text-muted-foreground">
            {description.generated_at
              ? t('bridge.describe.noteAt', {
                  when: formatRelative(description.generated_at, lang),
                })
              : t('bridge.generate.localNote')}
          </p>
          <p
            id={textId}
            className={
              clamps && !expanded
                ? 'line-clamp-4 text-[13px] whitespace-pre-line'
                : 'text-[13px] whitespace-pre-line'
            }
          >
            {description.text}
          </p>
          {clamps ? (
            <div>
              <Button
                variant="link"
                size="sm"
                className="h-auto p-0"
                aria-expanded={expanded}
                aria-controls={textId}
                onClick={() => setExpanded((v) => !v)}
              >
                {expanded ? t('bridge.describe.showLess') : t('bridge.describe.showMore')}
              </Button>
            </div>
          ) : null}
        </>
      ) : null}
      {showJob && job ? <GenerateStatus job={job} uiUrl={uiUrl} /> : null}
      {(showJob && canCancel) || offer ? (
        <div className="flex flex-wrap gap-2">
          {offer ? (
            <Button variant="outline" size="sm" onClick={() => setOpen(true)}>
              <PenLine aria-hidden="true" />
              {t('bridge.describe.action')}
            </Button>
          ) : null}
          {job && canCancel ? (
            <JobCancelButton job={job} ariaLabel={t('bridge.describe.cancelAria')} />
          ) : null}
        </div>
      ) : null}
      <DescribeDialog open={open} onOpenChange={setOpen} book={book} />
    </JobSection>
  );
}

/**
 * The confirmation before `sidecars.describe`: what it does, what it reads (the
 * notes the Bridge kept from generating this book, no new speech recognition),
 * that it costs a little agent time, and where the result appears.
 */
function DescribeDialog({
  open,
  onOpenChange,
  book,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  book: AdminBook;
}) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [busy, setBusy] = useState(false);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (busy) return;
    setBusy(true);
    const ok = await queueJob(
      qc,
      { kind: KIND_DESCRIBE, library_id: book.library_id, path: book.path, params: {} },
      { name: book.title || book.path, online: true },
    );
    setBusy(false);
    if (ok) onOpenChange(false);
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        title={t('bridge.describe.dialog')}
        description={book.title || book.path}
        icon={PenLine}
        tone="info"
      >
        <form onSubmit={(e) => void submit(e)} className="contents" noValidate>
          <DialogBody className="flex flex-col gap-3 text-[13px]">
            <p>{t('bridge.describe.dialogBody')}</p>
            <p className="text-muted-foreground">{t('bridge.describe.dialogCost')}</p>
            <p className="text-muted-foreground">{t('bridge.describe.dialogWhere')}</p>
          </DialogBody>
          <DialogFormFooter busy={busy} submitLabel={t('bridge.describe.submit')} />
        </form>
      </DialogContent>
    </Dialog>
  );
}
