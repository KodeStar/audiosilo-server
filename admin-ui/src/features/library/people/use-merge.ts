import { useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { api } from '@/api/client';
import { invalidateBooks } from '@/api/hooks';
import type { AdminBook, MergeSuggestion } from '@/api/types';
import { toastError } from '@/lib/errors';
import { formatNumber } from '@/lib/format';
import { toast } from '@/lib/toast';
import {
  mergeSteps,
  otherSpellings,
  undoSteps,
  type BulkStep,
  type PersonField,
} from './people-model';

/** Every book whose `field` is exactly `name` (in one library, or all), page by page. */
async function booksTagged(field: PersonField, name: string, libraryId?: number) {
  const out: AdminBook[] = [];
  let cursor: string | undefined;
  do {
    const filter = field === 'author' ? { author: name } : { narrator: name };
    const page = await api.adminBooks({ ...filter, library_id: libraryId, limit: 200, cursor });
    out.push(...page.books);
    cursor = page.next_cursor || undefined;
  } while (cursor);
  return out;
}

async function run(steps: BulkStep[]) {
  for (const s of steps) await api.bulkEdit(s.books, s.edit);
}

/**
 * Merging spellings of one person: collect the books tagged with the other
 * spellings, set the suggested one on each (an edit, so files are untouched),
 * and toast with an Undo that puts every book back as it was. `merging` is the
 * suggestion in flight (by its suggested name).
 */
export function useMerge(field: PersonField, libraryId?: number) {
  const { t, i18n } = useTranslation();
  const qc = useQueryClient();
  const [merging, setMerging] = useState<string | null>(null);
  const lang = i18n.resolvedLanguage ?? 'en';
  const counted = (n: number) => ({ count: n, formatted: formatNumber(n, lang) });

  const undo = async (books: AdminBook[]) => {
    try {
      await run(undoSteps(books, field));
      toast.add({
        title: t('people-merge.undone'),
        description: t('people-merge.undoneBody', counted(books.length)),
        type: 'success',
      });
    } catch (err) {
      toastError(t('people-merge.undoFailed'), err);
    } finally {
      invalidateBooks(qc, books);
    }
  };

  const merge = async (s: MergeSuggestion) => {
    setMerging(s.suggested);
    let books: AdminBook[] = [];
    try {
      for (const name of otherSpellings(s)) {
        books.push(...(await booksTagged(field, name, libraryId)));
      }
      // The filter is exact, but a book that already carries the suggested
      // spelling has nothing to merge (and nothing to undo).
      books = books.filter((b) => b[field] !== s.suggested);
      if (books.length === 0) {
        toast.add({ title: t('people-merge.nothing'), type: 'info' });
        return;
      }
      await run(mergeSteps(books, field, s.suggested));
      const merged = books;
      toast.add({
        title: t('people-merge.done', { name: s.suggested }),
        description: t('people-merge.doneBody', counted(books.length)),
        type: 'success',
        actionProps: { children: t('people-merge.undo'), onClick: () => void undo(merged) },
      });
    } catch (err) {
      toastError(t('people-merge.failed', { name: s.suggested }), err);
    } finally {
      setMerging(null);
      invalidateBooks(qc, books);
    }
  };

  return { merging, merge };
}
