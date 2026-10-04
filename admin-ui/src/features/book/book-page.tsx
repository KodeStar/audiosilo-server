import { useState } from 'react';
import { Link, useBlocker, useNavigate, useSearch } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { BookX, TriangleAlert } from 'lucide-react';
import { ApiError } from '@/api/client';
import { useAdminBook } from '@/api/hooks';
import type { AdminBookDetail, OverrideField } from '@/api/types';
import { EmptyState } from '@/components/empty-state';
import { Page } from '@/components/page';
import { QueryError } from '@/components/query-error';
import { Button, buttonVariants } from '@/components/ui/button';
import { Dialog, DialogContent, DialogFooter } from '@/components/ui/dialog';
import { AddToShareDialog } from '@/features/library/add-to-share-dialog';
import { refKey } from '@/lib/book-route';
import { BookAside } from './book-aside';
import { BookHero } from './book-hero';
import { commitDraft, draftErrors, type Drafts } from './book-model';
import { ChaptersCard } from './chapters-card';
import { DetailsCard } from './details-card';
import { DiskSection, FilesCard } from './files-card';
import { HERO_GRID, PAGE_GRID } from './layout';
import { MatchDialog } from './match-dialog';
import { SaveBar, SaveDiffDialog } from './save-changes';

/** A book, addressed by library + path: its metadata (edited in place), chapters, files and access. */
export function BookPage() {
  const { library, path, match } = useSearch({ from: '/library/book' });
  if (!library || !path) return <BookNotFound />;
  // A fresh page (drafts and all) per book.
  return (
    <BookLoader
      key={refKey({ library_id: library, path })}
      libraryId={library}
      path={path}
      match={!!match}
    />
  );
}

function BookLoader({
  libraryId,
  path,
  match,
}: {
  libraryId: number;
  path: string;
  match: boolean;
}) {
  const { t } = useTranslation();
  const detail = useAdminBook(libraryId, path);
  if (detail.isError) {
    if (detail.error instanceof ApiError && detail.error.status === 404) return <BookNotFound />;
    return (
      <Page>
        <QueryError
          title={t('book.error')}
          error={detail.error}
          onRetry={() => void detail.refetch()}
        />
      </Page>
    );
  }
  if (!detail.data) return <BookSkeleton />;
  return <BookView detail={detail.data} matchOnOpen={match} />;
}

function BookNotFound() {
  const { t } = useTranslation();
  return (
    <Page>
      <EmptyState
        icon={BookX}
        title={t('book.notFound.title')}
        body={t('book.notFound.body')}
        action={
          <Link
            to="/library/{-$section}"
            params={{ section: undefined }}
            className={buttonVariants({ variant: 'outline' })}
          >
            {t('book.notFound.back')}
          </Link>
        }
      />
    </Page>
  );
}

/** Shaped like the page: the hero, then the cards and the aside. */
function BookSkeleton() {
  const { t } = useTranslation();
  return (
    <div role="status" aria-label={t('common.loading')}>
      <div className="border-b">
        <div className={HERO_GRID}>
          <div className="skel aspect-square w-[min(240px,66vw)] rounded-[7px] md:w-auto" />
          <div className="flex flex-col gap-3">
            <span className="skel h-3 w-40" />
            <span className="skel h-11 w-[min(520px,100%)]" />
            <span className="skel h-4 w-64" />
            <span className="skel mt-3 h-9 w-72" />
          </div>
        </div>
      </div>
      <Page className="pt-6 md:pt-7">
        <div className={PAGE_GRID}>
          <div className="flex flex-col gap-5">
            <span className="skel h-[420px] rounded-xl" />
            <span className="skel h-[220px] rounded-xl" />
          </div>
          <div className="flex flex-col gap-4">
            <span className="skel h-[160px] rounded-xl" />
            <span className="skel h-[140px] rounded-xl" />
          </div>
        </div>
      </Page>
    </div>
  );
}

function BookView({ detail, matchOnOpen }: { detail: AdminBookDetail; matchOnOpen: boolean }) {
  const { t } = useTranslation();
  const navigate = useNavigate({ from: '/library/book' });
  const [drafts, setDrafts] = useState<Drafts>({});
  // Values the server refused at the last save, by field (cleared once that field changes).
  const [refused, setRefused] = useState<Partial<Record<OverrideField, string>>>({});
  const [reviewing, setReviewing] = useState(false);
  const [matching, setMatchingState] = useState(matchOnOpen);
  // Closing a dialog a link opened (?match=1) drops the param, so a reload doesn't reopen it.
  const setMatching = (open: boolean) => {
    setMatchingState(open);
    if (!open && matchOnOpen) {
      void navigate({ search: (s) => ({ library: s.library, path: s.path }), replace: true });
    }
  };
  const [sharing, setSharing] = useState(false);
  const count = Object.keys(drafts).length;
  const dirty = count > 0;

  const local = draftErrors(drafts);
  const errors: Partial<Record<OverrideField, string>> = { ...refused };
  for (const [f, key] of Object.entries(local)) errors[f as OverrideField] = t(key);

  const commit = (field: OverrideField, raw: string) => {
    setDrafts((d) => commitDraft(d, field, raw, detail.fields[field].value));
    setRefused((r) => {
      const next = { ...r };
      delete next[field];
      return next;
    });
  };
  /** Back to the saved book: no drafts, no refusals. */
  const reset = () => {
    setDrafts({});
    setRefused({});
  };
  const openMatch = () => setMatching(true);

  return (
    <>
      <BookHero
        detail={detail}
        matchBlocked={dirty}
        onMatch={openMatch}
        onAddToShare={() => setSharing(true)}
      />
      <Page className="pt-6 md:pt-7">
        <div className={PAGE_GRID}>
          <div className="flex min-w-0 flex-col gap-5">
            <DetailsCard detail={detail} drafts={drafts} errors={errors} onCommit={commit} />
            <ChaptersCard detail={detail} />
            <FilesCard detail={detail} />
            <DiskSection detail={detail} />
          </div>
          <BookAside
            detail={detail}
            matchBlocked={dirty}
            onMatch={openMatch}
            onAddToShare={() => setSharing(true)}
          />
        </div>
      </Page>
      {dirty ? (
        <SaveBar
          count={count}
          invalid={Object.keys(local).length}
          onDiscard={reset}
          onReview={() => setReviewing(true)}
        />
      ) : null}
      <SaveDiffDialog
        open={reviewing}
        onOpenChange={setReviewing}
        detail={detail}
        drafts={drafts}
        onSaved={reset}
        onRefused={(field, message) => setRefused((r) => ({ ...r, [field]: message }))}
      />
      <MatchDialog open={matching} onOpenChange={setMatching} detail={detail} />
      <AddToShareDialog
        open={sharing}
        onOpenChange={setSharing}
        books={[detail.book]}
        alreadyIn={new Set((detail.shares ?? []).map((s) => s.share_id))}
      />
      <LeaveGuard dirty={dirty} count={count} />
    </>
  );
}

/** Unsaved edits: ask before leaving the page (and the browser's own prompt before unloading). */
function LeaveGuard({ dirty, count }: { dirty: boolean; count: number }) {
  const { t } = useTranslation();
  const blocker = useBlocker({
    shouldBlockFn: () => true,
    disabled: !dirty,
    enableBeforeUnload: dirty,
    withResolver: true,
  });
  const open = blocker.status === 'blocked';
  return (
    <Dialog open={open} onOpenChange={(o) => (o ? undefined : blocker.reset?.())}>
      <DialogContent
        icon={TriangleAlert}
        tone="danger"
        title={t('book.leave.title')}
        description={t('book.leave.description', { count })}
      >
        <DialogFooter>
          <Button variant="ghost" onClick={() => blocker.reset?.()}>
            {t('book.leave.stay')}
          </Button>
          <Button variant="destructive" onClick={() => blocker.proceed?.()}>
            {t('book.leave.discard')}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
