import { useEffect, useMemo, useRef } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '@/api/client';
import { invalidateIssues, keys } from '@/api/hooks';
import { isOpen } from '@/features/bridge/bridge-model';
import { generatePollMs, KIND_DETECT } from '@/features/bridge/generate-model';
import { newestJobByBook } from './issues-model';

/**
 * How many of the newest chapter detections the list reads: the server's most
 * in one page. A book's open job is among the newest, as claims take the
 * oldest queued first.
 */
const DETECT_JOBS = 200;

/**
 * Health's chapter detections (BRIDGE-08): the newest `chapters.detect` jobs,
 * by book (newestJobByBook), read once for the whole list rather than per row,
 * asked again about every 10 s while one works and every minute while one is
 * parked (generatePollMs), only while the list is shown. Under the job lists'
 * key, so queueing from the list reads them again. When a job ends, the book
 * may have its chapters: the issue lists and counts are read again. Undefined
 * while not enabled or not loaded.
 */
export function useDetectJobs(enabled: boolean) {
  const qc = useQueryClient();
  const query = useQuery({
    queryKey: [...keys.bridgeJobLists, 'health', KIND_DETECT] as const,
    queryFn: () => api.bridgeJobs({ kind: KIND_DETECT, limit: DETECT_JOBS }),
    enabled,
    refetchInterval: (q) => generatePollMs(q.state.data?.jobs),
  });
  const jobs = query.data?.jobs;
  const open = useRef<ReadonlySet<number> | undefined>(undefined);
  useEffect(() => {
    if (!jobs) return;
    const before = open.current;
    const now = new Set(jobs.filter((j) => isOpen(j.state)).map((j) => j.id));
    open.current = now;
    if (before && [...before].some((id) => !now.has(id))) invalidateIssues(qc);
  }, [jobs, qc]);
  return useMemo(() => (jobs ? newestJobByBook(jobs) : undefined), [jobs]);
}
