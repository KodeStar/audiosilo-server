import { useState } from 'react';
import { keepPreviousData, useQuery, useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { api } from '@/api/client';
import { keys } from '@/api/hooks';
import { toastError } from '@/lib/errors';
import { JOB_POLL_MS, jobPollMs } from './librivox-model';

/** The LibriVox works matching q (none asked for a blank one). */
export function useLibrivoxSearch(q: string, enabled: boolean) {
  const query = q.trim();
  return useQuery({
    queryKey: keys.librivoxSearch(query),
    queryFn: () => api.librivoxSearch(query),
    enabled: enabled && query !== '',
    placeholderData: keepPreviousData,
  });
}

/**
 * One work's LibriVox recordings, asked again every few seconds while one of
 * them has an open job (its state, then where it landed).
 */
export function useLibrivoxWork(id: string | undefined) {
  return useQuery({
    queryKey: keys.librivoxWork(id ?? ''),
    queryFn: () => api.librivoxWork(id ?? ''),
    enabled: Boolean(id),
    refetchInterval: (q) =>
      q.state.data?.recordings.some((r) => r.job !== null) ? JOB_POLL_MS : false,
  });
}

/** A job this page queued or saw, by id, asked again every few seconds while it is open. */
export function useTrackedJob(id: number | undefined) {
  return useQuery({
    queryKey: keys.bridgeJob(id ?? 0),
    queryFn: () => api.bridgeJob(id ?? 0),
    enabled: id !== undefined,
    refetchInterval: (q) => jobPollMs(q.state.data),
  });
}

/**
 * Add: queues the project's fetch into a library. The work is asked again
 * either way (an in_library refusal shows where the recording already is); a
 * refusal is toasted.
 */
export function useAddLibrivox(workId: string) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [busy, setBusy] = useState<number>();
  const add = async (libraryId: number, librivoxId: number) => {
    setBusy(librivoxId);
    try {
      const { job } = await api.librivoxFetch(libraryId, librivoxId);
      qc.setQueryData(keys.bridgeJob(job.id), job);
      void qc.invalidateQueries({ queryKey: keys.bridgeJobLists });
      return job;
    } catch (err) {
      toastError(t('librivox.add.failed'), err);
      return undefined;
    } finally {
      setBusy(undefined);
      void qc.invalidateQueries({ queryKey: keys.librivoxWork(workId) });
    }
  };
  return { add, busy };
}
