import { useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { api, type BookListParams } from '@/api/client';
import { invalidateBooks, invalidateIssues } from '@/api/hooks';
import type { AdminBook, MergeSuggestion } from '@/api/types';
import { toastError } from '@/lib/errors';
import { counted } from '@/lib/format';
import { toast } from '@/lib/toast';
import {
  mergeSteps,
  otherSpellings,
  undoSteps,
  type BulkStep,
  type MergeField,
} from './people-model';

/** Every book `filter` lists that `keep` keeps, page by page. */
export async function booksWhere(filter: BookListParams, keep: (b: AdminBook) => boolean) {
  const out: AdminBook[] = [];
  let cursor: string | undefined;
  do {
    const page = await api.adminBooks({ ...filter, limit: 200, cursor });
    out.push(...page.books.filter(keep));
    cursor = page.next_cursor || undefined;
  } while (cursor);
  return out;
}

/**
 * Every book whose `field` is exactly `name` (in one library, or all). The filter
 * also finds the co-credits naming `name` ("A, B" for B); a merge rewrites the
 * whole field, so those are left out, keeping their other people.
 */
function booksTagged(field: MergeField, name: string, libraryId?: number) {
  return booksWhere({ [field]: name, library_id: libraryId }, (b) => b[field] === name);
}

async function run(steps: BulkStep[]) {
  for (const s of steps) await api.bulkEdit(s.books, s.edit);
}

/**
 * Merging spellings of one person (or series): collect the books tagged with the other
 * spellings, set the suggested one on each (an edit, so files are untouched),
 * and toast with an Undo that puts every book back as it was. `merging` is the
 * suggestion in flight (by its suggested name).
 */
export function useMerge(field: MergeField, libraryId?: number) {
  const { t, i18n } = useTranslation();
  const qc = useQueryClient();
  const [merging, setMerging] = useState<string | null>(null);
  const lang = i18n.resolvedLanguage ?? 'en';

  const undo = async (books: AdminBook[]) => {
    try {
      await run(undoSteps(books, field));
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

  /**
   * Merges `s`. `tagged` collects the books carrying a spelling (by default each
   * other spelling's, looked up one by one; a series' come in one lookup).
   */
  const merge = async (s: MergeSuggestion, tagged?: () => Promise<AdminBook[]>) => {
    setMerging(s.suggested);
    let books: AdminBook[] = [];
    try {
      // The spellings at once; each one's pages in turn (a cursor needs the page before).
      const found = tagged
        ? await tagged()
        : (
            await Promise.all(otherSpellings(s).map((name) => booksTagged(field, name, libraryId)))
          ).flat();
      // A book that already carries the suggested spelling has nothing to merge
      // (and nothing to undo).
      books = found.filter((b) => b[field] !== s.suggested);
      if (books.length === 0) {
        toast.add({ title: t('people-merge.nothing'), type: 'info' });
        return;
      }
      await run(mergeSteps(books, field, s.suggested));
      toast.add({
        title: t('people-merge.done', { name: s.suggested }),
        description: t('people-merge.doneBody', counted(books.length, lang)),
        type: 'success',
        actionProps: { children: t('people-merge.undo'), onClick: () => void undo(books) },
      });
    } catch (err) {
      toastError(t('people-merge.failed', { name: s.suggested }), err);
    } finally {
      setMerging(null);
      invalidateBooks(qc, books);
      invalidateIssues(qc);
    }
  };

  return { merging, merge };
}
