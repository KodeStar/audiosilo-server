import type { QueryClient } from '@tanstack/react-query';
import i18n from 'i18next';
import { api } from '@/api/client';
import { noteScanStarted } from '@/api/hooks';
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
      noteScanStarted(qc, library.id);
      toast.add({
        title: t('palette.toast.rescanning', { name: library.name }),
        description: t('palette.toast.rescanningSub'),
        type: 'info',
      });
    },
    (err: unknown) => toastError(t('palette.toast.rescanFailed', { name: library.name }), err),
  );
}

/** Queues a scan of every library (Health's "Check again"); the queue runs them one at a time. */
export function rescanAll(qc: QueryClient, libraries: { id: number; name: string }[]) {
  const t = i18n.t;
  Promise.all(libraries.map((l) => api.scanLibrary(l.id))).then(
    () => {
      for (const l of libraries) noteScanStarted(qc, l.id);
      toast.add({
        title: t('health.toast.checking', { count: libraries.length }),
        description: t('health.toast.checkingBody'),
        type: 'info',
      });
    },
    (err: unknown) => toastError(t('health.toast.checkFailed'), err),
  );
}
