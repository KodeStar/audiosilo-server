import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { RotateCw, Sparkles } from 'lucide-react';
import { useServerInfo } from '@/api/hooks';
import type { AdminBook } from '@/api/types';
import { Button } from '@/components/ui/button';
import { isOpen, jobActions } from './bridge-model';
import { GenerateDialog } from './generate-dialog';
import { generateAction, generateAvailable, generateJobsKnown } from './generate-model';
import { JobCancelButton, JobSection } from './generate-parts';
import { GenerateStatus } from './generate-status';
import { useBookGenerateJob, useBridgeLink, useGenerateEstimate } from './use-generate';

/**
 * The Community card's characters-and-recaps part: the book's generate job
 * while one is open (its stage, ETA, cost; why it's parked, with a link into
 * the Bridge) or how the last one ended, and the action that opens the
 * confirmation. Offered only while an online Bridge has the sidecars feature
 * and metadata is on, and not when the community already has both characters
 * and recaps. Renders nothing when there is nothing to show.
 */
export function GenerateSection({ book }: { book: AdminBook }) {
  const { t } = useTranslation();
  const server = useServerInfo().data;
  const available = generateAvailable(server);
  const { job } = useBookGenerateJob(book.library_id, book.path, generateJobsKnown(server));
  // The brief estimate: enough for what the book has and so which action to offer.
  const estimate = useGenerateEstimate(book.library_id, book.path, available, { brief: true });
  const parked = job?.state === 'needs_attention';
  const uiUrl = useBridgeLink(parked);
  const [open, setOpen] = useState(false);

  const has = estimate.data?.has;
  const action = has ? generateAction(has) : undefined;
  const generated = Boolean(has && (has.local.characters || has.local.recaps));
  const jobOpen = job ? isOpen(job.state) : false;
  // The action: with an answer that offers it, no job open, and not while the
  // Bridge is away (it couldn't run it).
  const offer = available && !jobOpen && action !== undefined && action !== 'hidden';
  // A cancelled job says nothing worth a line.
  const showJob = job && job.state !== 'cancelled';
  // An open dialog stays (it says when the Bridge went away), whatever else changed.
  if (!open && !showJob && !offer && !generated) return null;

  const canCancel = job ? jobActions(job).cancel : false;

  return (
    <JobSection
      id="generate-title"
      title={t('bridge.generate.title')}
      job={showJob ? job : undefined}
    >
      {generated && !jobOpen ? (
        <p className="text-[12.5px] text-muted-foreground">{t('bridge.generate.localNote')}</p>
      ) : null}
      {showJob && job ? <GenerateStatus job={job} uiUrl={uiUrl} /> : null}
      {(showJob && canCancel) || offer ? (
        <div className="flex flex-wrap gap-2">
          {offer ? (
            <Button variant="outline" size="sm" onClick={() => setOpen(true)}>
              {action === 'regenerate' ? (
                <RotateCw aria-hidden="true" />
              ) : (
                <Sparkles aria-hidden="true" />
              )}
              {action === 'regenerate'
                ? t('bridge.generate.regenerate')
                : t('bridge.generate.action')}
            </Button>
          ) : null}
          {job && canCancel ? (
            <JobCancelButton job={job} ariaLabel={t('bridge.generate.cancelAria')} />
          ) : null}
        </div>
      ) : null}
      <GenerateDialog
        open={open}
        onOpenChange={setOpen}
        book={{ library_id: book.library_id, path: book.path, title: book.title }}
        regenerate={action === 'regenerate'}
        offline={!available}
      />
    </JobSection>
  );
}
