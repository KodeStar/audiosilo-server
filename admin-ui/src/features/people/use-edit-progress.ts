import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { api } from '@/api/client';
import { invalidateProgress } from '@/api/hooks';
import type { BookRef, ProgressEdit } from '@/api/types';
import { toastError } from '@/lib/errors';
import { toast } from '@/lib/toast';

/** What a progress menu acts on: a person's progress on one book. */
export interface ProgressTarget extends BookRef {
  userId: number;
  username: string;
  title: string;
  finished: boolean;
  position: number;
  started_at?: string | null;
  finished_at?: string | null;
}

/**
 * Saves an admin's edit to someone's progress and refreshes what shows it. Marking
 * a book finished offers Undo (back to where they were). Returns whether it saved.
 */
export function useEditProgress() {
  const { t } = useTranslation();
  const qc = useQueryClient();
  return async (p: ProgressTarget, edit: ProgressEdit, done: string, undo?: ProgressEdit) => {
    try {
      await api.editProgress(p.library_id, p.path, p.userId, edit);
      invalidateProgress(qc, p.userId, p);
      toast.add({
        title: done,
        type: 'success',
        ...(undo
          ? {
              actionProps: {
                children: t('progress.undo'),
                onClick: () =>
                  void api
                    .editProgress(p.library_id, p.path, p.userId, undo)
                    .then(() => invalidateProgress(qc, p.userId, p))
                    .catch((err: unknown) => toastError(t('progress.failed'), err)),
              },
            }
          : {}),
      });
      return true;
    } catch (err) {
      toastError(t('progress.failed'), err);
      return false;
    }
  };
}
