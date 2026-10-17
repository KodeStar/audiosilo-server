import { useRef, useState } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { AudioLines } from 'lucide-react';
import { api, ApiError } from '@/api/client';
import { keys } from '@/api/hooks';
import type { AdminBook } from '@/api/types';
import { FactList } from '@/components/fact-list';
import { Dialog, DialogBody, DialogContent, DialogFormFooter } from '@/components/ui/dialog';
import { FormError } from '@/components/ui/field';
import {
  ForBooks,
  lengthRow,
  NoEstimate,
  NotWritableNotice,
  OfflineNotice,
} from '@/features/bridge/generate-parts';
import { detectPlan, KIND_DETECT } from '@/features/bridge/generate-model';
import { queueInOrder, queueReport } from '@/features/bridge/use-generate';
import { counted, formatHours } from '@/lib/format';
import { toast } from '@/lib/toast';

/**
 * Health's Detect chapters (Bridge Phase 1d): the confirmation before queueing
 * `chapters.detect` for one book or a selection. It says what happens (the
 * Bridge transcribes the audio and finds the chapters in the narration), that
 * no agent is used, the transcription time the Bridge's figures predict, and
 * what happens when no chapters are heard. It reads the Bridge's status (its
 * figures and library mappings) rather than an estimate per book, so a large
 * selection costs one request. Nothing is queued while a book's library can't
 * be written to; a book whose generate job is open is passed over (that run
 * finds its chapters itself), any other failure stops the run and says how far
 * it got (queueInOrder). It stays open while a run queues (Cancel, Escape and
 * the close button do nothing then), so the run's outcome, its error or its
 * clearing of the bar's selection, lands in the dialog that started it, never
 * in one opened since.
 */
export function DetectChaptersDialog({
  books,
  onOpenChange,
  onQueued,
}: {
  /** The books to queue it for; null closes the dialog. */
  books: AdminBook[] | null;
  onOpenChange: (open: boolean) => void;
  /** Called once the books were queued, before the dialog closes. */
  onQueued?: () => void;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const qc = useQueryClient();
  const open = books !== null;
  const status = useQuery({ queryKey: keys.bridge, queryFn: api.bridge, enabled: open });
  const [busy, setBusy] = useState(false);
  // A run in flight, read synchronously (a close or a second press can come
  // before the re-render that shows busy).
  const running = useRef(false);
  const [error, setError] = useState('');
  const chosen = books ?? [];
  const plan = detectPlan(status.data, chosen);
  const offline = status.data ? !status.data.online : false;
  const length = chosen.reduce((sum, b) => sum + Math.max(0, b.duration), 0);

  const close = (next: boolean) => {
    if (!next && running.current) return;
    if (!next) setError('');
    onOpenChange(next);
  };

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (running.current || !chosen.length) return;
    running.current = true;
    setBusy(true);
    setError('');
    const out = await queueInOrder(
      qc,
      chosen.map((b) => ({
        kind: KIND_DETECT,
        library_id: b.library_id,
        path: b.path,
        params: {},
      })),
      // The book's generate job finds its chapters itself.
      { skip: (err) => err instanceof ApiError && err.code === 'generate_open' },
    );
    running.current = false;
    setBusy(false);
    const failed = queueReport(out, chosen.length, t, lang);
    if (failed !== undefined) {
      setError(failed);
      return;
    }
    toast.add(
      out.queued
        ? {
            title: t('health.toast.detectQueued', counted(out.queued, lang)),
            description: out.skipped
              ? t('health.toast.detectSkipped', counted(out.skipped, lang))
              : t('health.toast.detectQueuedBody'),
            type: 'success',
          }
        : { title: t('health.toast.detectSkipped', counted(out.skipped, lang)) },
    );
    onQueued?.();
    close(false);
  };

  return (
    <Dialog open={open} onOpenChange={close}>
      <DialogContent
        title={t('health.detect.title')}
        description={
          chosen.length > 1
            ? t('health.detect.books', counted(chosen.length, lang))
            : chosen[0]?.title || chosen[0]?.path
        }
        icon={AudioLines}
        tone="info"
      >
        <form onSubmit={(e) => void submit(e)} className="contents" noValidate>
          <DialogBody className="flex flex-col gap-3 text-[13px]">
            {plan.notWritable ? <NotWritableNotice /> : null}
            {offline ? <OfflineNotice /> : null}
            <p>{t('health.detect.body')}</p>
            <FactList
              rows={[
                ...lengthRow(t, lang, length),
                [
                  t('bridge.generate.asr'),
                  status.isPending ? undefined : plan.asrHours === null ? (
                    <NoEstimate />
                  ) : (
                    <ForBooks count={chosen.length}>
                      {t('bridge.generate.about', {
                        what: formatHours(plan.asrHours * 3600, lang),
                      })}
                    </ForBooks>
                  ),
                ],
                [t('bridge.generate.cost'), t('health.detect.noAgent')],
              ]}
            />
            <p className="text-[12.5px] text-muted-foreground">{t('health.detect.notFound')}</p>
            <FormError>{error}</FormError>
          </DialogBody>
          <DialogFormFooter
            busy={busy}
            disabled={plan.notWritable || !chosen.length}
            submitLabel={t('health.detect.submit')}
          />
        </form>
      </DialogContent>
    </Dialog>
  );
}
