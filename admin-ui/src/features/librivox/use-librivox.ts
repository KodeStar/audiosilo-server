import { useState } from 'react';
import { keepPreviousData, useQuery, useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { ApiError, api } from '@/api/client';
import { keys } from '@/api/hooks';
import { toastError } from '@/lib/errors';
import { toast } from '@/lib/toast';

/**
 * The LibriVox works matching q (none asked for a blank one). Catalogue data
 * with no local state: a minute old is fresh enough for a refocus or a retyped
 * query.
 */
export function useLibrivoxSearch(q: string, enabled: boolean) {
  const query = q.trim();
  return useQuery({
    queryKey: keys.librivoxSearch(query),
    queryFn: () => api.librivoxSearch(query),
    enabled: enabled && query !== '',
    placeholderData: keepPreviousData,
    staleTime: 60_000,
  });
}

/**
 * One work's LibriVox recordings. Not polled: each row follows its own job
 * (useTrackedJob) and asks for the work again when that job ends.
 */
export function useLibrivoxWork(id: string | undefined) {
  return useQuery({
    queryKey: keys.librivoxWork(id ?? ''),
    queryFn: () => api.librivoxWork(id ?? ''),
    enabled: Boolean(id),
  });
}

/**
 * Add: queues the project's fetch into a library. The work is asked again
 * either way (an in_library refusal shows where the recording already is); a
 * refusal is toasted (not_found, a project the metadata no longer lists, in
 * the console's words).
 */
export function useAddLibrivox(workId: string) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [busy, setBusy] = useState(false);
  const add = async (libraryId: number, librivoxId: number) => {
    setBusy(true);
    try {
      const { job } = await api.librivoxFetch(libraryId, librivoxId);
      // The lists first: the job's own key sits under theirs, and the answer
      // just seeded is fresh.
      void qc.invalidateQueries({ queryKey: keys.bridgeJobLists });
      qc.setQueryData(keys.bridgeJob(job.id), job);
      return job;
    } catch (err) {
      // not_found here is the project the metadata no longer lists (the
      // generic code's sentence would be the server's English one).
      if (err instanceof ApiError && err.code === 'not_found') {
        toast.add({
          title: t('librivox.add.failed'),
          description: t('librivox.add.notFound'),
          type: 'error',
        });
      } else {
        toastError(t('librivox.add.failed'), err);
      }
      return undefined;
    } finally {
      setBusy(false);
      void qc.invalidateQueries({ queryKey: keys.librivoxWork(workId) });
    }
  };
  return { add, busy };
}
