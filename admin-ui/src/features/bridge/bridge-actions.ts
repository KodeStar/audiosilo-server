import type { QueryClient } from '@tanstack/react-query';
import i18n from 'i18next';
import { api } from '@/api/client';
import { keys } from '@/api/hooks';
import type { BridgeJob, BridgeJobRequest } from '@/api/types';
import { toastError } from '@/lib/errors';
import { toast } from '@/lib/toast';
import { kindName } from './bridge-model';

// What the Bridge page's buttons do: queue a job and cancel one, each saying how
// it went in a toast and refreshing the job lists.

/**
 * Queues a job and says so: queued (and whether an offline Bridge will run it
 * later), already waiting (the open job came back instead), or still stopping
 * (the open job that came back is one being cancelled, so nothing new is queued
 * until it has stopped). A failure, kind_unavailable among them, is a toast.
 * Resolves whether it was queued (or is already waiting).
 */
export async function queueJob(
  qc: QueryClient,
  req: BridgeJobRequest,
  ctx: { name: string; online: boolean },
): Promise<boolean> {
  const t = i18n.t;
  const words = { what: kindName(t, req.kind), name: ctx.name };
  try {
    const { job, created } = await api.queueBridgeJob(req);
    if (created) {
      toast.add({
        title: t('bridge.queue.done', words),
        description: ctx.online ? t('bridge.queue.doneBody') : t('bridge.queue.offlineBody'),
        type: 'success',
      });
      return true;
    }
    if (job.cancel_requested) {
      toast.add({
        title: t('bridge.queue.stopping', words),
        description: t('bridge.queue.stoppingBody'),
        type: 'warning',
      });
      return false;
    }
    toast.add({
      title: t('bridge.queue.already', words),
      description: t('bridge.queue.alreadyBody'),
    });
    return true;
  } catch (err) {
    toastError(t('bridge.queue.failed'), err);
    return false;
  } finally {
    void qc.invalidateQueries({ queryKey: keys.bridgeJobLists });
  }
}

/**
 * Cancels a job: one not started yet is cancelled at once, a claimed or running
 * one is asked to stop and stops at the Bridge's next report.
 */
export async function cancelJob(qc: QueryClient, job: BridgeJob): Promise<void> {
  const t = i18n.t;
  try {
    const next = await api.cancelBridgeJob(job.id);
    toast.add(
      next.state === 'cancelled'
        ? { title: t('bridge.cancel.done'), type: 'success' }
        : { title: t('bridge.cancel.asked'), description: t('bridge.cancel.askedBody') },
    );
  } catch (err) {
    toastError(t('bridge.cancel.failed'), err);
  } finally {
    void qc.invalidateQueries({ queryKey: keys.bridgeJobLists });
  }
}
