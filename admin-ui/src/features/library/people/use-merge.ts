import { useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { api } from '@/api/client';
import { invalidateBooks, invalidateIssues } from '@/api/hooks';
import type { MergedBook, MergeField, MergeUndone } from '@/api/types';
import { errorMessage, toastError } from '@/lib/errors';
import { counted, formatList } from '@/lib/format';
import { toast } from '@/lib/toast';

/** How many paths a partly undone merge's toast names. */
const SKIPPED_SHOWN = 3;

/** A toast action that runs once, however often it is clicked: a second Undo
 * would find the books already put back and report them as changed. */
function once(run: () => Promise<void>) {
  let ran = false;
  return () => {
    if (ran) return;
    ran = true;
    void run();
  };
}

/** The in-flight spelling drives the merge button. Undo uses the prior
 * overrides returned by the committed merge. */
export function useMerge(field: MergeField, libraryId?: number) {
  const { t, i18n } = useTranslation();
  const qc = useQueryClient();
  const [merging, setMerging] = useState<string | null>(null);
  const lang = i18n.resolvedLanguage ?? 'en';

  /**
   * Puts the books back in one server transaction. A book a scan moved, or
   * whose field was edited since, stays as it is and is named; the rest go
   * back. A failed Undo changed nothing and is offered again.
   */
  const undo = async (target: string, books: MergedBook[]) => {
    let done: MergeUndone;
    try {
      done = await api.undoMerge(field, target, books);
    } catch (err) {
      toast.add({
        title: t('people-merge.undoFailed'),
        description: errorMessage(err, t),
        type: 'error',
        actionProps: { children: t('people-merge.undo'), onClick: once(() => undo(target, books)) },
      });
      return;
    } finally {
      invalidateBooks(qc, books);
      invalidateIssues(qc); // Health's series spellings
    }
    if (done.skipped.length === 0) {
      toast.add({
        title: t('people-merge.undone'),
        description: t('people-merge.undoneBody', counted(done.restored, lang)),
        type: 'success',
      });
      return;
    }
    const shown = done.skipped.slice(0, SKIPPED_SHOWN).map((b) => b.path);
    const more = done.skipped.length - shown.length;
    if (more > 0) shown.push(t('people-merge.more', counted(more, lang)));
    toast.add({
      title: done.restored > 0 ? t('people-merge.undonePartly') : t('people-merge.undoFailed'),
      description: t('people-merge.skipped', {
        ...counted(done.skipped.length, lang),
        paths: formatList(shown, lang),
      }),
      type: 'warning',
      timeout: 0,
    });
  };

  const merge = async (target: string, sources: string[]) => {
    setMerging(target);
    let books: MergedBook[] = [];
    try {
      ({ books } = await api.mergeSpellings(field, sources, target, libraryId));
      if (books.length === 0) {
        toast.add({ title: t('people-merge.nothing'), type: 'info' });
        return;
      }
      toast.add({
        title: t('people-merge.done', { name: target }),
        description: t('people-merge.doneBody', counted(books.length, lang)),
        type: 'success',
        actionProps: { children: t('people-merge.undo'), onClick: once(() => undo(target, books)) },
      });
    } catch (err) {
      toastError(t('people-merge.failed', { name: target }), err);
    } finally {
      setMerging(null);
      invalidateBooks(qc, books);
      invalidateIssues(qc);
    }
  };

  return { merging, merge };
}
