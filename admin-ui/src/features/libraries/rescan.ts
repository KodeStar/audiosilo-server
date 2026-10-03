import type { QueryClient } from '@tanstack/react-query';
import i18n from 'i18next';
import { api } from '@/api/client';
import { keys } from '@/api/hooks';
import { toastError } from '@/lib/errors';
import { toast } from '@/lib/toast';

/**
 * Starts a rescan of a library from anywhere (its card, the palette). The
 * server reports it running before answering, so refetching the library list
 * is all it takes for every view to show the progress (the list polls while a
 * scan runs). A plain function, not a mutation hook: the palette closes on
 * select, and the toast must still appear when the request settles.
 */
export function rescanLibrary(qc: QueryClient, library: { id: number; name: string }) {
  const t = i18n.t;
  api.scanLibrary(library.id).then(
    () => {
      void qc.invalidateQueries({ queryKey: keys.libraries });
      toast.add({
        title: t('palette.toast.rescanning', { name: library.name }),
        description: t('palette.toast.rescanningSub'),
        type: 'info',
      });
    },
    (err: unknown) => toastError(t('palette.toast.rescanFailed', { name: library.name }), err),
  );
}
