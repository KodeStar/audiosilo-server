import { useId, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { Bot, Hourglass, Sparkles } from 'lucide-react';
import type { BridgeEstimate } from '@/api/types';
import { FactList } from '@/components/fact-list';
import { Notice } from '@/components/notice';
import { QueryError } from '@/components/query-error';
import { Checkbox } from '@/components/ui/checkbox';
import { Dialog, DialogBody, DialogContent, DialogFormFooter } from '@/components/ui/dialog';
import { FormError } from '@/components/ui/field';
import { formatHours, formatNumber, formatUsd } from '@/lib/format';
import { toast } from '@/lib/toast';
import {
  destinationKey,
  estimateView,
  generatePlan,
  planEstimate,
  type Contribute,
} from './generate-model';
import {
  ContributeSwitch,
  ForBooks,
  lengthRow,
  NoEstimate,
  NotWritableNotice,
  OfflineNotice,
} from './generate-parts';
import { queueInOrder, queueReport, useGenerateEstimate } from './use-generate';

/**
 * Generate characters and recaps for one book: what it will read (the audio,
 * or the ebook found beside it), the transcription and agent cost the Bridge's
 * history predicts, where the results appear, a contribute choice (this server
 * only by default; offered only when the Bridge can contribute), and the
 * earlier books of the series without recaps, queued first. Nothing can be
 * queued while the Bridge can't write to the library. The Bridge going offline
 * while it is open (`offline`) doesn't close it: it says so, and what is queued
 * waits for the Bridge.
 */
export function GenerateDialog({
  open,
  onOpenChange,
  book,
  regenerate,
  offline = false,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  book: { library_id: number; path: string; title: string };
  regenerate: boolean;
  offline?: boolean;
}) {
  const { t } = useTranslation();
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        title={regenerate ? t('bridge.generate.dialogRegen') : t('bridge.generate.dialog')}
        description={book.title || book.path}
        icon={Sparkles}
        tone="community"
      >
        {open ? (
          <GenerateForm
            book={book}
            regenerate={regenerate}
            offline={offline}
            onDone={() => onOpenChange(false)}
          />
        ) : null}
      </DialogContent>
    </Dialog>
  );
}

function GenerateForm({
  book,
  regenerate,
  offline,
  onDone,
}: {
  book: { library_id: number; path: string; title: string };
  regenerate: boolean;
  offline: boolean;
  onDone: () => void;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const qc = useQueryClient();
  // The full estimate: the dialog quotes its source, earlier books and cost.
  const estimate = useGenerateEstimate(book.library_id, book.path, true, { brief: false });
  const [contribute, setContribute] = useState<Contribute>('local');
  const [includeEarlier, setIncludeEarlier] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const e = estimate.data;
  const earlier = e?.earlier ?? [];
  // Contributing only when the Bridge can (the server refuses it otherwise).
  const contributeAs: Contribute = e?.can_contribute ? contribute : 'local';
  const plan = generatePlan([...(includeEarlier ? earlier : []), book], contributeAs);
  const canConfirm = e !== undefined && !estimateView(e).notWritable;

  const submit = async (ev: React.FormEvent) => {
    ev.preventDefault();
    if (!canConfirm || busy) return;
    setBusy(true);
    setError(undefined);
    const out = await queueInOrder(qc, plan);
    setBusy(false);
    const failed = queueReport(out, plan.length, t, lang);
    if (failed !== undefined) {
      setError(failed);
      return;
    }
    toast.add({
      title: t('bridge.generate.queued'),
      description:
        plan.length > 1
          ? t('bridge.generate.queuedMany', {
              count: plan.length,
              n: formatNumber(plan.length, lang),
            })
          : t('bridge.generate.queuedBody'),
      type: 'success',
    });
    onDone();
  };

  return (
    <form onSubmit={(ev) => void submit(ev)} className="contents" noValidate>
      <DialogBody className="flex flex-col gap-4">
        {estimate.isError ? (
          <QueryError
            title={t('bridge.generate.estimateError')}
            error={estimate.error}
            onRetry={() => void estimate.refetch()}
          />
        ) : !e ? (
          <div className="skel h-40 rounded-lg" role="status" aria-label={t('common.loading')} />
        ) : (
          <EstimateBody
            estimate={e}
            regenerate={regenerate}
            offline={offline}
            contribute={contributeAs}
            onContribute={setContribute}
            includeEarlier={includeEarlier}
            onIncludeEarlier={setIncludeEarlier}
          />
        )}
        <FormError>{error}</FormError>
      </DialogBody>
      <DialogFormFooter
        busy={busy}
        disabled={!canConfirm}
        submitLabel={
          plan.length > 1
            ? t('bridge.generate.submitMany', {
                count: plan.length,
                n: formatNumber(plan.length, lang),
              })
            : t('bridge.generate.submit')
        }
      />
    </form>
  );
}

/** The estimate's facts, the choices, and what warns or stops. */
function EstimateBody({
  estimate,
  regenerate,
  offline,
  contribute,
  onContribute,
  includeEarlier,
  onIncludeEarlier,
}: {
  estimate: BridgeEstimate;
  regenerate: boolean;
  offline: boolean;
  contribute: Contribute;
  onContribute: (value: Contribute) => void;
  includeEarlier: boolean;
  onIncludeEarlier: (on: boolean) => void;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const id = useId();
  const v = estimateView(estimate);
  const earlier = estimate.earlier;
  // The figures quote every run the button queues: this book and the earlier
  // ones it takes first, naming how many runs that is.
  const plan = planEstimate(estimate, includeEarlier);

  return (
    <>
      {v.notWritable ? <NotWritableNotice /> : null}
      {offline ? <OfflineNotice /> : null}
      {v.agentNotReady ? (
        <Notice tone="warn" icon={Bot} title={t('bridge.generate.agentNotReady')}>
          {t('bridge.generate.agentNotReadyBody')}
        </Notice>
      ) : v.noFigures ? (
        <Notice tone="info" icon={Hourglass} title={t('bridge.generate.noFigures')}>
          {t('bridge.generate.noFiguresBody')}
        </Notice>
      ) : null}
      <FactList
        rows={[
          [
            t('bridge.generate.source'),
            v.ebook ? (
              <span className="flex flex-col">
                <span>{t('bridge.generate.sourceEbook')}</span>
                <span
                  className="font-mono text-[12px] font-normal text-muted-foreground"
                  title={v.ebook.path}
                >
                  {v.ebook.name}
                </span>
              </span>
            ) : (
              t('bridge.generate.sourceAudio')
            ),
          ],
          ...lengthRow(t, lang, estimate.duration_seconds),
          [
            t('bridge.generate.asr'),
            plan.asrHours === 0 ? (
              t('bridge.generate.asrNone')
            ) : plan.asrHours === null ? (
              <NoEstimate />
            ) : (
              <ForBooks count={plan.books}>
                {t('bridge.generate.about', { what: formatHours(plan.asrHours * 3600, lang) })}
              </ForBooks>
            ),
          ],
          [
            t('bridge.generate.cost'),
            plan.costUsd === null ? (
              <NoEstimate />
            ) : (
              <ForBooks count={plan.books}>
                {v.subscription
                  ? t('bridge.generate.costUsage', { cost: formatUsd(plan.costUsd, lang) })
                  : t('bridge.generate.about', { what: formatUsd(plan.costUsd, lang) })}
              </ForBooks>
            ),
          ],
          [t('bridge.generate.where'), t(destinationKey(contribute))],
        ]}
      />
      {v.subscription ? (
        <p className="text-[12.5px] text-muted-foreground">
          {t('bridge.generate.subscriptionNote')}
        </p>
      ) : null}
      {regenerate ? (
        <p className="text-[12.5px] text-muted-foreground">{t('bridge.generate.replaces')}</p>
      ) : null}
      {v.canContribute ? <ContributeSwitch value={contribute} onChange={onContribute} /> : null}
      {earlier.length ? (
        <div className="flex flex-col gap-2 rounded-lg border px-4 py-3">
          <label htmlFor={`${id}-earlier`} className="flex cursor-pointer items-start gap-2.5">
            <Checkbox
              id={`${id}-earlier`}
              className="mt-0.5"
              checked={includeEarlier}
              onCheckedChange={(on) => onIncludeEarlier(on)}
            />
            <span className="flex min-w-0 flex-col gap-0.5">
              <span className="font-semibold">
                {t('bridge.generate.earlier', {
                  count: earlier.length,
                  n: formatNumber(earlier.length, lang),
                })}
              </span>
              <span className="text-[12.5px] text-muted-foreground">
                {t('bridge.generate.earlierBody')}
              </span>
            </span>
          </label>
          <ol className="ml-7 flex flex-col gap-0.5 text-[12.5px]">
            {earlier.map((b) => (
              <li key={b.path} className="flex min-w-0 gap-2">
                {b.series_index ? (
                  <span className="w-8 shrink-0 text-right text-muted-foreground tabular-nums">
                    {b.series_index}
                  </span>
                ) : null}
                <span className="min-w-0 [overflow-wrap:anywhere]">{b.title || b.path}</span>
              </li>
            ))}
          </ol>
        </div>
      ) : null}
    </>
  );
}
