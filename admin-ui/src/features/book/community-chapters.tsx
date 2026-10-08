import { useEffect, useRef, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { ChevronDown, ChevronUp, Globe, LoaderCircle, RefreshCw } from 'lucide-react';
import { api } from '@/api/client';
import { invalidateBooks, invalidateIssues, settleBookEdit, useServerInfo } from '@/api/hooks';
import type { AdminBookDetail, BookEditRequest } from '@/api/types';
import { Button } from '@/components/ui/button';
import { toastError } from '@/lib/errors';
import { counted, formatDuration } from '@/lib/format';
import { toast } from '@/lib/toast';
import { say } from '@/lib/phrase';
import { communityState } from './book-model';

/**
 * The community's chapters for this book, at the top of the Chapters card: where the
 * chapters come from, what the community offers (detailed chapters, other titles,
 * chapters for a book with none) or why it offers nothing, and a check on demand.
 * A check runs in the background; the page follows it (useAdminBook) to the end.
 */
export function CommunityChapters({ detail }: { detail: AdminBookDetail }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const qc = useQueryClient();
  const server = useServerInfo();
  const [review, setReview] = useState(false);
  const [busy, setBusy] = useState(false);
  const b = detail.book;
  const checking = detail.community_checking;
  const state = communityState(detail, (s) => formatDuration(s, lang));

  // A check that just finished may have changed the chapters the lists count.
  const wasChecking = useRef(checking);
  useEffect(() => {
    if (wasChecking.current && !checking) {
      invalidateBooks(qc);
      invalidateIssues(qc);
    }
    wasChecking.current = checking;
  }, [checking, qc]);

  const canCheck = !!server.data?.capabilities.metadata && b.matched;
  if (!state && !checking) return null;

  const edit = async (req: BookEditRequest, done: string, count = 1) => {
    setBusy(true);
    try {
      settleBookEdit(qc, await api.editBook(b.library_id, b.path, req));
      invalidateIssues(qc);
      toast.add({ title: t(done, counted(count, lang)), type: 'success' });
    } catch (err) {
      toastError(t('book.community.editFailed'), err);
    } finally {
      setBusy(false);
    }
  };

  const check = async () => {
    setBusy(true);
    try {
      settleBookEdit(qc, await api.checkCommunityChapters(b.library_id, b.path));
    } catch (err) {
      toastError(t('book.community.checkFailed'), err);
    } finally {
      setBusy(false);
    }
  };

  const titles = state?.titles ?? [];
  const takeTitles = (list: typeof titles) =>
    void edit(
      { chapters: { set: Object.fromEntries(list.map((x) => [x.index, x.community])) } },
      'book.community.titlesSaved',
      list.length,
    );

  return (
    <section
      aria-labelledby="community-chapters"
      className="flex flex-col gap-2.5 rounded-[12px] border bg-muted/40 p-3.5"
    >
      <div className="flex flex-wrap items-start gap-x-3 gap-y-2">
        <Globe className="mt-0.5 size-4 shrink-0 text-prov-community" aria-hidden="true" />
        <div className="flex min-w-0 flex-1 basis-60 flex-col gap-1">
          <h3 id="community-chapters" className="text-[13px] font-semibold">
            {t('book.community.label')}
          </h3>
          <p className="text-[13.5px] text-muted-foreground" aria-live="polite">
            {checking ? (
              <span className="inline-flex items-center gap-1.5">
                <LoaderCircle className="size-3.5 animate-spin" aria-hidden="true" />
                {t('book.community.checking')}
              </span>
            ) : state ? (
              say(t, state.line, lang)
            ) : null}
          </p>
          {!checking
            ? state?.notes.map((n) => (
                <p key={n.key} className="text-[12.5px] text-subtle-foreground">
                  {say(t, n, lang)}
                </p>
              ))
            : null}
        </div>
        <div className="flex shrink-0 flex-wrap items-center gap-1.5">
          {!checking && state?.action ? (
            <Button
              variant="outline"
              size="sm"
              disabled={busy}
              onClick={() =>
                void edit({ chapter_source: state.action!.source }, 'book.community.switched')
              }
            >
              {t(state.action.label)}
            </Button>
          ) : null}
          {!checking && titles.length ? (
            <Button variant="outline" size="sm" onClick={() => setReview(!review)}>
              {t(review ? 'book.community.hideTitles' : 'book.community.reviewTitles')}
              {review ? <ChevronUp aria-hidden="true" /> : <ChevronDown aria-hidden="true" />}
            </Button>
          ) : null}
          {canCheck ? (
            <Button
              variant="ghost"
              size="sm"
              disabled={busy || checking}
              onClick={() => void check()}
            >
              <RefreshCw aria-hidden="true" />
              {t(detail.community_chapters ? 'book.community.recheck' : 'book.community.check')}
            </Button>
          ) : null}
        </div>
      </div>
      {review && titles.length && !checking ? (
        <div className="flex flex-col gap-1">
          <ol className="flex flex-col">
            {titles.map((x) => (
              <li
                key={x.index}
                className="grid grid-cols-[28px_minmax(0,1fr)_auto] items-center gap-3 rounded-[10px] px-2.5 py-1.5 text-[13.5px] hover:bg-muted"
              >
                <span className="text-[12px] text-subtle-foreground tabular-nums">
                  {x.index + 1}
                </span>
                <span className="flex min-w-0 flex-col">
                  <span className="truncate font-medium">{x.community}</span>
                  <span className="truncate text-[12px] text-subtle-foreground">
                    {t('book.community.now', { title: x.current })}
                  </span>
                </span>
                <Button
                  variant="ghost"
                  size="sm"
                  disabled={busy}
                  aria-label={t('book.community.takeTitle', { title: x.community })}
                  onClick={() => takeTitles([x])}
                >
                  {t('book.community.take')}
                </Button>
              </li>
            ))}
          </ol>
          {titles.length > 1 ? (
            <div>
              <Button
                variant="outline"
                size="sm"
                disabled={busy}
                onClick={() => takeTitles(titles)}
              >
                {t('book.community.takeAll', counted(titles.length, lang))}
              </Button>
            </div>
          ) : null}
        </div>
      ) : null}
    </section>
  );
}
