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
      // Out of the lists at once, so its card (and the unmatched list it may
      // have open) unmounts and its detail query is left unobserved for the
      // cache to drop. Removing that query while the card still showed it made
      // the card fetch the deleted import again, a 404 (server#396).
      qc.setQueriesData<Import[]>({ queryKey: keys.importLists }, (old) =>
        old?.filter((o) => o.id !== imp.id),
      );
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
