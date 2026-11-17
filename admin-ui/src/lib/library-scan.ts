import type { AdminLibrary } from '@/api/types';

/** Whether a library's scan runs or waits in the job queue. */
export function scanActive(l: Pick<AdminLibrary, 'scan'>): boolean {
  return l.scan.running || !!l.scan.queued;
}
