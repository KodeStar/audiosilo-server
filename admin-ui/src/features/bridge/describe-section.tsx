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
import { isOpen } from './bridge-model';
import {
  clampsDescription,
  describeOffered,
  generateAvailable,
  generateJobsKnown,
  KIND_DESCRIBE,
} from './generate-model';
import { JobActions, JobSection, OfflineNotice } from './generate-parts';
import { GenerateStatus } from './generate-status';
import { useBookGenerateJob, useBridgeLink, useGenerateEstimate } from './use-generate';

/**
 * The Community card's description part (Bridge Phase 1c): the description the
 * Bridge generated for the book (its text, clamped, with Show more, and a note
 * when the player's About shows another description instead), the describe job
 * while one is open or how it failed, and "Write a description", which queues
 * `sidecars.describe` after a confirmation. Offered only when the Bridge can
 * write one (describeOffered) and nothing would be shown in its place: no
 * description generated here (as this card shows it) and none of the book's
 * own. Renders nothing when there is nothing to show.
 */
export function DescribeSection({
  book,
  description,
  ownDescription = false,
}: {
  book: AdminBook;
  description?: GeneratedDescription;
  /** The book has a description of its own, which the player's About shows before a generated one. */
  ownDescription?: boolean;
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

  // Never beside a description the card shows, nor from an estimate being
  // fetched again (a job that just ended changed what the book has).
  const offer =
    available &&
    !description &&
    !ownDescription &&
    !estimate.isFetching &&
    describeOffered(estimate.data, jobs);
  // A cancelled job says nothing, and once a description shows neither does a
  // finished one: a done job is the description itself, a failed one was made
  // good since (a later generate job wrote one).
  const showJob = job && job.state !== 'cancelled' && !(description && !isOpen(job.state));
  if (!open && !showJob && !offer && !description) return null;

  const clamps = description ? clampsDescription(description.text) : false;
  // The player's About shows another description first: the book's own, or the
  // community's (which /meta serves instead).
  const shownElsewhere = ownDescription || Boolean(estimate.data?.has.community.description);

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
          {shownElsewhere ? (
            <p className="text-[12.5px] text-muted-foreground">{t('bridge.describe.notShown')}</p>
          ) : null}
        </>
      ) : null}
      {showJob && job ? <GenerateStatus job={job} uiUrl={uiUrl} /> : null}
      <JobActions job={job} cancelAria={t('bridge.describe.cancelAria')}>
        {offer ? (
          <Button variant="outline" size="sm" onClick={() => setOpen(true)}>
            <PenLine aria-hidden="true" />
            {t('bridge.describe.action')}
          </Button>
        ) : null}
      </JobActions>
      <DescribeDialog open={open} onOpenChange={setOpen} book={book} offline={!available} />
    </JobSection>
  );
}

/**
 * The confirmation before `sidecars.describe`: what it does, what it reads (the
 * notes the Bridge kept from generating this book, no new speech recognition),
 * that it costs a little agent time, and where the result appears. The Bridge
 * going away while it is open is said, as the generate dialog says it: the job
 * still queues, and waits.
 */
function DescribeDialog({
  open,
  onOpenChange,
  book,
  offline,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  book: AdminBook;
  offline: boolean;
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
      { name: book.title || book.path, online: !offline },
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
            {offline ? <OfflineNotice /> : null}
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
