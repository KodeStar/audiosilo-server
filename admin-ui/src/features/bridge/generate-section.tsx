import { useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { RotateCw, Sparkles, X } from 'lucide-react';
import { useServerInfo } from '@/api/hooks';
import type { AdminBook } from '@/api/types';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { cancelJob } from './bridge-actions';
import { isOpen, jobActions, stateLook } from './bridge-model';
import { GenerateDialog } from './generate-dialog';
import { generateAction, generateAvailable, generateJobsKnown } from './generate-model';
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
  const qc = useQueryClient();
  const server = useServerInfo().data;
  const available = generateAvailable(server);
  const { job } = useBookGenerateJob(book.library_id, book.path, generateJobsKnown(server));
  // The brief estimate: enough for what the book has and so which action to offer.
  const estimate = useGenerateEstimate(book.library_id, book.path, available, { brief: true });
  const parked = job?.state === 'needs_attention';
  const uiUrl = useBridgeLink(parked);
  const [open, setOpen] = useState(false);
  const [cancelling, setCancelling] = useState(false);

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

  const look = job ? stateLook(job) : undefined;
  const canCancel = job ? jobActions(job).cancel : false;

  return (
    <section className="flex flex-col gap-2.5 border-t pt-3.5" aria-labelledby="generate-title">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h4 id="generate-title" className="text-[13px] font-semibold">
          {t('bridge.generate.title')}
        </h4>
        {showJob && look ? <Badge variant={look.tone}>{t(look.key)}</Badge> : null}
      </div>
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
            <Button
              variant="ghost"
              size="sm"
              disabled={cancelling}
              onClick={() => {
                setCancelling(true);
                void cancelJob(qc, job).finally(() => setCancelling(false));
              }}
              aria-label={t('bridge.generate.cancelAria')}
            >
              <X aria-hidden="true" />
              {t('bridge.jobs.cancel')}
            </Button>
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
    </section>
  );
}
