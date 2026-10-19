import { useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { LoaderCircle, Send } from 'lucide-react';
import { useServerInfo } from '@/api/hooks';
import type { AdminBook } from '@/api/types';
import { Button } from '@/components/ui/button';
import { queueJob } from './bridge-actions';
import {
  contributionsOn,
  generateAvailable,
  generateJobsKnown,
  KIND_CONTRIBUTE,
  shareOffered,
  shareRequest,
} from './generate-model';
import { JobActions, JobSection } from './generate-parts';
import { GenerateStatus } from './generate-status';
import { useBookGenerateJob, useBridgeLink, useGenerateEstimate } from './use-generate';

/**
 * The Community card's "Share with the community" part (BRIDGE-PLAN.md Phase
 * 2b, contribute later): after a run kept on this server, one click queues
 * `sidecars.contribute`, which files the community issues from the characters
 * and recaps the Bridge already generated (nothing is generated again). The
 * share job while it works, or how it ended with its issue links. Offered only
 * while the contributions module is on and the Bridge can contribute now, and
 * only for what the community still lacks (shareOffered). A share job that
 * exists shows with its Cancel whatever the flag says now (the flag is also off
 * while the Bridge is away, and a job exists only if the module was on); only a
 * new Share needs it. Renders nothing when there is nothing to show.
 */
export function ShareSection({ book }: { book: AdminBook }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const server = useServerInfo().data;
  const on = contributionsOn(server);
  const available = generateAvailable(server);
  const { job, jobs } = useBookGenerateJob(
    book.library_id,
    book.path,
    generateJobsKnown(server),
    KIND_CONTRIBUTE,
  );
  // The same brief estimate the characters-and-recaps part reads (one query).
  const estimate = useGenerateEstimate(book.library_id, book.path, on && available, {
    brief: true,
  });
  const uiUrl = useBridgeLink(job?.state === 'needs_attention');
  const [busy, setBusy] = useState(false);

  // Never from an estimate being fetched again (a job that just ended changed what the book has).
  const dimensions = estimate.isFetching ? [] : shareOffered(server, estimate.data, jobs);
  const offer = dimensions.length > 0;
  // A cancelled job says nothing worth a line.
  const showJob = job && job.state !== 'cancelled';
  if (!offer && !showJob) return null;

  const share = async () => {
    setBusy(true);
    await queueJob(qc, shareRequest(book, dimensions), {
      name: book.title || book.path,
      online: available,
    });
    setBusy(false);
  };

  return (
    <JobSection id="share-title" title={t('bridge.share.title')} job={showJob ? job : undefined}>
      {offer ? (
        <p className="text-[12.5px] text-muted-foreground">
          {t(`bridge.share.offer.${dimensions.length === 2 ? 'both' : dimensions[0]}`)}
        </p>
      ) : null}
      {showJob && job ? <GenerateStatus job={job} uiUrl={uiUrl} /> : null}
      <JobActions job={job} cancelAria={t('bridge.share.cancelAria')}>
        {offer ? (
          <Button variant="outline" size="sm" disabled={busy} onClick={() => void share()}>
            {busy ? (
              <LoaderCircle className="animate-spin" aria-hidden="true" />
            ) : (
              <Send aria-hidden="true" />
            )}
            {t('bridge.share.action')}
          </Button>
        ) : null}
      </JobActions>
    </JobSection>
  );
}
