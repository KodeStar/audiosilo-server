import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { BookOpenText, CircleCheck, RotateCw, TriangleAlert } from 'lucide-react';
import { useServerInfo } from '@/api/hooks';
import type { AdminBookDetail } from '@/api/types';
import { Button } from '@/components/ui/button';
import { Card, CardHeader } from '@/components/ui/card';
import { KIND_DETECT, KIND_READALONG } from '@/features/bridge/generate-model';
import { JobActions } from '@/features/bridge/generate-parts';
import { GenerateStatus } from '@/features/bridge/generate-status';
import { useBookGenerateJob, useBridgeLink } from '@/features/bridge/use-generate';
import { formatPercent, formatRelative } from '@/lib/format';
import { ReadalongDialog } from './readalong-dialog';
import {
  readalongAvailable,
  readalongCardShown,
  readalongJobsKnown,
  readalongView,
} from './readalong-model';

/**
 * The book page's Read-along card (Bridge Phase 3c), for a book with an ebook:
 * whether listeners can follow the narration in the ebook (a ready map, with
 * how much of the text it lines up and when it was made), why they can't any
 * more (a stale map: the ebook or the audio changed since), the book's
 * read-along job while one is open or how the last one ended, and Make (or
 * Make again) with its confirmation, per book only. While a generate or
 * chapter detection job holds the book's audio on the Bridge its status shows
 * where the offer would be (the server refuses a read-along until it has
 * finished). Renders nothing for a book with no ebook, or when there is
 * nothing to show (no map, no job, no Bridge that makes read-alongs).
 */
export function ReadalongCard({ detail }: { detail: AdminBookDetail }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const b = detail.book;
  const server = useServerInfo().data;
  const available = readalongAvailable(server);
  // When the job ends the book is fetched again (useBookGenerateJob: a kind that
  // writes the book's files), so its new map's state shows here.
  const { job, jobs } = useBookGenerateJob(
    b.library_id,
    b.path,
    readalongJobsKnown(server, b),
    KIND_READALONG,
  );
  const [open, setOpen] = useState(false);
  const view = readalongView(b, job, jobs, available);
  const uiUrl = useBridgeLink(job?.state === 'needs_attention');
  // A cancelled job says why only when the server or the Bridge did.
  const showJob = job && (job.state !== 'cancelled' || job.error);
  if (!open && !readalongCardShown(b, view, showJob ? job : undefined)) return null;

  return (
    <Card aria-labelledby="readalong-title">
      <CardHeader
        titleId="readalong-title"
        title={t('book.readalong.title')}
        description={t('book.readalong.summary')}
      />
      <div className="flex flex-col gap-3 p-5">
        {view.map === 'ready' ? (
          <p className="flex items-start gap-2 text-[13.5px]">
            <CircleCheck className="mt-0.5 size-4 shrink-0 text-success" aria-hidden="true" />
            <span>
              {view.coverage !== undefined
                ? t('book.readalong.ready', { percent: formatPercent(view.coverage, lang) })
                : t('book.readalong.readyPlain')}
              {view.generatedAt ? (
                <span className="text-muted-foreground">
                  {' · '}
                  {t('book.readalong.made', { when: formatRelative(view.generatedAt, lang) })}
                </span>
              ) : null}
            </span>
          </p>
        ) : view.map === 'stale' ? (
          <p className="flex items-start gap-2 text-[13.5px]">
            <TriangleAlert className="mt-0.5 size-4 shrink-0 text-warning" aria-hidden="true" />
            <span>{t(view.staleKey ?? 'book.readalong.stale.other')}</span>
          </p>
        ) : (
          <p className="flex items-start gap-2 text-[13.5px] text-muted-foreground">
            <BookOpenText className="mt-0.5 size-4 shrink-0" aria-hidden="true" />
            {t('book.readalong.none')}
          </p>
        )}
        {view.blocker ? (
          <>
            <p className="text-[12.5px] text-muted-foreground">
              {view.blocker.kind === KIND_DETECT
                ? t('book.readalong.afterDetect')
                : t('book.readalong.afterGenerate')}
            </p>
            <GenerateStatus job={view.blocker} />
          </>
        ) : showJob && job ? (
          <GenerateStatus job={job} uiUrl={uiUrl} />
        ) : null}
        <JobActions job={job} cancelAria={t('book.readalong.cancelAria')}>
          {view.action ? (
            <Button variant="outline" size="sm" onClick={() => setOpen(true)}>
              {view.action === 'remake' ? (
                <RotateCw aria-hidden="true" />
              ) : (
                <BookOpenText aria-hidden="true" />
              )}
              {view.action === 'remake' ? t('book.readalong.remake') : t('book.readalong.make')}
            </Button>
          ) : null}
        </JobActions>
      </div>
      <ReadalongDialog
        open={open}
        onOpenChange={setOpen}
        book={{ library_id: b.library_id, path: b.path, title: b.title }}
        remake={view.map !== 'none'}
        offline={!available}
      />
    </Card>
  );
}
