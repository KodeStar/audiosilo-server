import { useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { api } from '@/api/client';
import { invalidateBooks, invalidateIssues } from '@/api/hooks';
import type { MergedBook, MergeField } from '@/api/types';
import { toastError } from '@/lib/errors';
import { counted } from '@/lib/format';
import { toast } from '@/lib/toast';
import { undoSteps } from './people-model';

/** The in-flight spelling drives the merge button. Undo uses the prior
 * overrides returned by the committed merge. */
export function useMerge(field: MergeField, libraryId?: number) {
  const { t, i18n } = useTranslation();
  const qc = useQueryClient();
  const [merging, setMerging] = useState<string | null>(null);
  const lang = i18n.resolvedLanguage ?? 'en';

  const undo = async (books: MergedBook[]) => {
    try {
      for (const step of undoSteps(books, field)) await api.bulkEdit(step.books, step.edit);
      toast.add({
        title: t('people-merge.undone'),
        description: t('people-merge.undoneBody', counted(books.length, lang)),
        type: 'success',
      });
    } catch (err) {
      toastError(t('people-merge.undoFailed'), err);
    } finally {
      invalidateBooks(qc, books);
      invalidateIssues(qc); // Health's series spellings
    }
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
        actionProps: { children: t('people-merge.undo'), onClick: () => void undo(books) },
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
