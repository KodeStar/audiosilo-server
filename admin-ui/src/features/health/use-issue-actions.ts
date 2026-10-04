import { useQueryClient } from '@tanstack/react-query';
import { useNavigate } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { api } from '@/api/client';
import { invalidateCover, invalidateIssues, settleBookEdit } from '@/api/hooks';
import type { AdminBook, BookRef, IssueKind } from '@/api/types';
import { bookRoute, refOf } from '@/lib/book-route';
import { toastError } from '@/lib/errors';
import { counted } from '@/lib/format';
import { toast } from '@/lib/toast';
import { FIXES } from './issues-model';

/**
 * What the Health page does to books: ignore them under a category (with Undo),
 * show them again, and each category's fix (open the book, its match dialog or
 * its folder's detection, or read its files again).
 */
export function useIssueActions(kind: IssueKind) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const qc = useQueryClient();
  const navigate = useNavigate();

  const unignore = async (refs: BookRef[], quiet = false) => {
    try {
      await api.unignoreIssue(kind, refs);
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
      await api.ignoreIssue(kind, refs);
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

  const rescan = async (b: AdminBook) => {
    try {
      const fresh = await api.rescanBook(b.library_id, b.path);
      settleBookEdit(qc, fresh);
      invalidateCover(qc, b.library_id, b.path);
      toast.add(
        fresh.book.scan_error
          ? {
              title: t('health.toast.stillBroken', { title: b.title || b.path }),
              description:
                fresh.book.scan_error_detail || t(`health.code.${fresh.book.scan_error}`),
              type: 'warning',
            }
          : { title: t('health.toast.fixed', { title: b.title || b.path }), type: 'success' },
      );
    } catch (err) {
      toastError(t('health.toast.rescanFailed', { title: b.title || b.path }), err);
    } finally {
      invalidateIssues(qc);
    }
  };

  const fix = (b: AdminBook) => {
    switch (FIXES[kind]) {
      case 'cover':
        return navigate(bookRoute(b.library_id, b.path));
      case 'match':
        return navigate({
          ...bookRoute(b.library_id, b.path),
          search: { library: b.library_id, path: b.path, match: true },
        });
      case 'folder':
        return navigate({
          to: '/library/{-$section}',
          params: { section: 'folders' },
          search: { library: b.library_id, folder: b.path },
        });
      case 'rescan':
        return rescan(b);
    }
  };

  return { ignore, unignore, fix, rescan };
}
