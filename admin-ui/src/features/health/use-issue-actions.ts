import { useRef, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useNavigate } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { BULK_LIMIT, api } from '@/api/client';
import { invalidateIssues, rescanBook, setFolderMode } from '@/api/hooks';
import type { AdminBook, BookRef, IssueKind } from '@/api/types';
import { bookRoute, refKey, refOf } from '@/lib/book-route';
import { toastError } from '@/lib/errors';
import { counted } from '@/lib/format';
import { relBaseName, relParent } from '@/lib/paths';
import { toast } from '@/lib/toast';
import { chunk } from '@/lib/utils';
import { FIXES } from './issues-model';

/** How many book re-reads a bulk "Read again" keeps in flight. */
const RESCAN_CONCURRENCY = 2;

/**
 * What the Health page does to books: ignore them under a category (with Undo),
 * show them again, and each category's fix (open the book, its match dialog or
 * its folder's detection, read its files again, or join its disc folders).
 */
export function useIssueActions(kind: IssueKind) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const qc = useQueryClient();
  const navigate = useNavigate();
  // The books whose fix (a re-read or a join) is in flight: the ref guards a second
  // click before the re-render lands, the state disables their fix buttons.
  const inFlight = useRef(new Set<string>());
  const [busy, setBusy] = useState<ReadonlySet<string>>(new Set());
  const markBusy = (key: string, on: boolean) => {
    if (on) inFlight.current.add(key);
    else inFlight.current.delete(key);
    setBusy(new Set(inFlight.current));
  };
  // Large selections go in batches of the server's limit.
  const send = async (fn: typeof api.ignoreIssue, refs: BookRef[]) => {
    for (const part of chunk(refs, BULK_LIMIT)) await fn(kind, part);
  };

  const unignore = async (refs: BookRef[], quiet = false) => {
    try {
      await send(api.unignoreIssue, refs.map(refOf));
      if (!quiet) {
        toast.add({
          title: t('health.toast.unignored', counted(refs.length, lang)),
          type: 'success',
        });
      }
    } catch (err) {
      toastError(t('health.toast.unignoreFailed'), err);
    } finally {
      invalidateIssues(qc);
    }
  };

  /** Ignores the books under this category, with an Undo in the toast. */
  const ignore = async (books: Pick<AdminBook, 'library_id' | 'path' | 'title'>[]) => {
    const refs = books.map(refOf);
    try {
      await send(api.ignoreIssue, refs);
      toast.add({
        title:
          books.length === 1
            ? t('health.toast.ignoredOne', { title: books[0].title || books[0].path })
            : t('health.toast.ignored', counted(books.length, lang)),
        description: t('health.toast.ignoredBody', { kind: t(`health.kind.${kind}`) }),
        type: 'success',
        actionProps: { children: t('health.undo'), onClick: () => void unignore(refs, true) },
      });
    } catch (err) {
      toastError(t('health.toast.ignoreFailed'), err);
    } finally {
      invalidateIssues(qc);
    }
  };

  const rescan = async (b: AdminBook, refreshIssues = true) => {
    const title = b.title || b.path;
    try {
      const fresh = await rescanBook(qc, b, refreshIssues);
      toast.add(
        fresh.book.scan_error
          ? {
              title: t('health.toast.stillBroken', { title }),
              description:
                fresh.book.scan_error_detail || t(`health.code.${fresh.book.scan_error}`),
              type: 'warning',
            }
          : { title: t('health.toast.fixed', { title }), type: 'success' },
      );
    } catch (err) {
      toastError(t('health.toast.rescanFailed', { title }), err);
    }
  };

  /**
   * Reads several books again, RESCAN_CONCURRENCY at a time: each is a synchronous
   * re-read on the server (outside its scan queue), and firing them all at once
   * would trip its per-client rate limit. The issues refresh once, at the end.
   */
  const rescanMany = async (books: AdminBook[]) => {
    let next = 0;
    const worker = async () => {
      while (next < books.length) await rescan(books[next++], false);
    };
    await Promise.all(Array.from({ length: Math.min(RESCAN_CONCURRENCY, books.length) }, worker));
    invalidateIssues(qc);
  };

  /**
   * Joins the disc folders a split book is listed by (its first disc) into one
   * book: "Always one book" on the folder holding them, which rescans the library.
   */
  const join = async (b: AdminBook) => {
    const folder = relParent(b.path);
    const name = relBaseName(folder);
    try {
      await setFolderMode(qc, b.library_id, folder, 'book');
      toast.add({
        title: t('health.toast.joined', { folder: name }),
        description: t('health.toast.joinedBody'),
        type: 'success',
      });
    } catch (err) {
      toastError(t('health.toast.joinFailed', { folder: name }), err);
    } finally {
      invalidateIssues(qc);
    }
  };

  /** Runs a book's async fix once at a time: a click while it is in flight does nothing. */
  const once = async (b: AdminBook, run: () => Promise<void>) => {
    const key = refKey(b);
    if (inFlight.current.has(key)) return;
    markBusy(key, true);
    try {
      await run();
    } finally {
      markBusy(key, false);
    }
  };

  const fix = (b: AdminBook) => {
    switch (FIXES[kind]) {
      case 'cover':
        return navigate(bookRoute(b.library_id, b.path));
      case 'match':
        return navigate({
          to: '/library/book',
          search: { library: b.library_id, path: b.path, match: true },
        });
      case 'folder':
        return navigate({
          to: '/library/{-$section}',
          params: { section: 'folders' },
          search: { library: b.library_id, folder: b.path },
        });
      case 'rescan':
        return once(b, () => rescan(b));
      case 'join':
        return once(b, () => join(b));
    }
  };

  /** Whether a book's fix is in flight (its fix button is disabled meanwhile). */
  const fixing = (b: BookRef) => busy.has(refKey(b));

  return { ignore, unignore, fix, fixing, rescan, rescanMany };
}

export type IssueActions = ReturnType<typeof useIssueActions>;
