import { useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { api } from '@/api/client';
import { keys } from '@/api/hooks';
import type { Import } from '@/api/types';
import { toastError } from '@/lib/errors';
import { toast } from '@/lib/toast';

/**
 * Discards an import that isn't applied (no confirm: nothing was written; it can
 * be fetched again). `pending` is true while a discard is under way.
 */
export function useDiscardImport() {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [pending, setPending] = useState(false);
  const discard = async (imp: Import) => {
    setPending(true);
    try {
      await api.deleteImport(imp.id);
      qc.removeQueries({ queryKey: keys.importDetail(imp.id) });
      toast.add({ title: t('imports.discarded', { user: imp.username }), type: 'success' });
    } catch (err) {
      toastError(t('imports.discardFailed'), err);
    } finally {
      setPending(false);
      void qc.invalidateQueries({ queryKey: keys.importLists });
    }
  };
  return { discard, pending };
}
