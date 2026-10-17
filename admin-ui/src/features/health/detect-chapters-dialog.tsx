import { useState } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { AudioLines, Unplug } from 'lucide-react';
import { api, ApiError } from '@/api/client';
import { keys } from '@/api/hooks';
import type { AdminBook } from '@/api/types';
import { FactList } from '@/components/fact-list';
import { Notice } from '@/components/notice';
import { Dialog, DialogBody, DialogContent, DialogFormFooter } from '@/components/ui/dialog';
import { FormError } from '@/components/ui/field';
import { NotWritableNotice } from '@/features/bridge/generate-parts';
import { detectPlan, KIND_DETECT } from '@/features/bridge/generate-model';
import { errorMessage } from '@/lib/errors';
import { counted, formatDuration, formatHours, formatNumber } from '@/lib/format';
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
 * it got.
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
  const [error, setError] = useState('');
  const chosen = books ?? [];
  const plan = detectPlan(status.data, chosen);
  const offline = status.data ? !status.data.online : false;
  const length = chosen.reduce((sum, b) => sum + Math.max(0, b.duration), 0);
  const many = chosen.length > 1;
  const forBooks = (what: string) =>
    many
      ? t('bridge.generate.forBooks', {
          count: chosen.length,
          n: formatNumber(chosen.length, lang),
          what,
        })
      : what;

  const close = (next: boolean) => {
    if (!next) setError('');
    onOpenChange(next);
  };

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (busy || !chosen.length) return;
    setBusy(true);
    setError('');
    let queued = 0;
    let skipped = 0;
    try {
      for (const b of chosen) {
        try {
          await api.queueBridgeJob({
            kind: KIND_DETECT,
            library_id: b.library_id,
            path: b.path,
            params: {},
          });
          queued++;
        } catch (err) {
          // The book's generate job finds its chapters itself.
          if (err instanceof ApiError && err.code === 'generate_open') skipped++;
          else throw err;
        }
      }
      toast.add(
        queued
          ? {
              title: t('health.toast.detectQueued', counted(queued, lang)),
              description: skipped
                ? t('health.toast.detectSkipped', counted(skipped, lang))
                : t('health.toast.detectQueuedBody'),
              type: 'success',
            }
          : { title: t('health.toast.detectSkipped', counted(skipped, lang)) },
      );
      onQueued?.();
      close(false);
    } catch (err) {
      const why = errorMessage(err, t);
      setError(
        queued
          ? t('bridge.generate.partial', {
              done: formatNumber(queued, lang),
              total: formatNumber(chosen.length, lang),
              error: why,
            })
          : why,
      );
    } finally {
      setBusy(false);
      void qc.invalidateQueries({ queryKey: keys.bridgeJobLists });
    }
  };

  const noEstimate = (
    <span className="text-muted-foreground">{t('bridge.generate.noEstimate')}</span>
  );
  return (
    <Dialog open={open} onOpenChange={close}>
      <DialogContent
        title={t('health.detect.title')}
        description={
          many
            ? t('health.detect.books', counted(chosen.length, lang))
            : chosen[0]?.title || chosen[0]?.path
        }
        icon={AudioLines}
        tone="info"
      >
        <form onSubmit={(e) => void submit(e)} className="contents" noValidate>
          <DialogBody className="flex flex-col gap-3 text-[13px]">
            {plan.notWritable ? <NotWritableNotice /> : null}
            {offline ? (
              <Notice tone="warn" icon={Unplug} title={t('bridge.generate.offline')}>
                {t('bridge.generate.offlineBody')}
              </Notice>
            ) : null}
            <p>{t('health.detect.body')}</p>
            <FactList
              rows={[
                ...(length > 0
                  ? [
                      [t('bridge.generate.length'), formatDuration(length, lang)] as [
                        string,
                        React.ReactNode,
                      ],
                    ]
                  : []),
                [
                  t('bridge.generate.asr'),
                  status.isPending
                    ? undefined
                    : plan.asrHours === null
                      ? noEstimate
                      : forBooks(
                          t('bridge.generate.about', {
                            what: formatHours(plan.asrHours * 3600, lang),
                          }),
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
