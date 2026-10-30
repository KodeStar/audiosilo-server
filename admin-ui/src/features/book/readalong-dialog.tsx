import { useRef, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { BookOpenText, Unplug } from 'lucide-react';
import { ApiError } from '@/api/client';
import type { ReadalongEstimate } from '@/api/types';
import { FactList } from '@/components/fact-list';
import { Notice } from '@/components/notice';
import { QueryError } from '@/components/query-error';
import { Dialog, DialogBody, DialogContent, DialogFormFooter } from '@/components/ui/dialog';
import { FormError } from '@/components/ui/field';
import { KIND_READALONG } from '@/features/bridge/generate-model';
import { NoEstimate, NotWritableNotice, OfflineNotice } from '@/features/bridge/generate-parts';
import {
  queueInOrder,
  queueReport,
  skipNotes,
  useReadalongEstimate,
} from '@/features/bridge/use-generate';
import { formatHours, formatRelative } from '@/lib/format';
import { relBaseName } from '@/lib/paths';
import { toast } from '@/lib/toast';
import {
  READALONG_BRIDGE_VERSION,
  readalongDialogView,
  type ReadalongBridge,
} from './readalong-model';

/**
 * Make (or Make again) a book's read-along (Bridge Phase 3c): the confirmation
 * before queueing `readalong.align` for one book. It says what happens (the
 * Bridge transcribes the audio, or reuses the transcript it kept, and lines up
 * each sentence of the ebook with the narration), the most transcription the
 * Bridge's figures predict ("less if the Bridge still has this book's
 * transcript"), that no agent is used, and that a poor match (another edition
 * or another book) stops the job with nothing saved. Nothing can be queued while
 * the Bridge can't write to the library, the book has no ebook, or the Bridge
 * is a release too old to make read-alongs (it says which to update to). The
 * Bridge going offline while it is open doesn't close it: it says so, and what
 * is queued waits for the Bridge. It stays open while the job is queued.
 */
export function ReadalongDialog({
  open,
  onOpenChange,
  book,
  remake,
  bridge,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  book: { library_id: number; path: string; title: string };
  /** The book has a map (ready or stale): Make again. */
  remake: boolean;
  /** Where the Bridge stands now (it can change while the dialog is open). */
  bridge: ReadalongBridge;
}) {
  const { t } = useTranslation();
  // A run in flight, read synchronously (a close can come before the re-render that shows busy).
  const running = useRef(false);
  const close = (next: boolean) => {
    if (!next && running.current) return;
    onOpenChange(next);
  };
  return (
    <Dialog open={open} onOpenChange={close}>
      <DialogContent
        title={remake ? t('book.readalong.dialogRemake') : t('book.readalong.dialog')}
        description={book.title || book.path}
        icon={BookOpenText}
        tone="info"
      >
        {open ? (
          <ReadalongForm
            book={book}
            remake={remake}
            bridge={bridge}
            running={running}
            onDone={() => onOpenChange(false)}
          />
        ) : null}
      </DialogContent>
    </Dialog>
  );
}

function ReadalongForm({
  book,
  remake,
  bridge,
  running,
  onDone,
}: {
  book: { library_id: number; path: string; title: string };
  remake: boolean;
  bridge: ReadalongBridge;
  running: React.RefObject<boolean>;
  onDone: () => void;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const qc = useQueryClient();
  const estimate = useReadalongEstimate(book.library_id, book.path);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const e = estimate.data;
  const view = e ? readalongDialogView(e, bridge) : undefined;
  const canConfirm = view?.canConfirm ?? false;

  const submit = async (ev: React.FormEvent) => {
    ev.preventDefault();
    if (!canConfirm || running.current) return;
    running.current = true;
    setBusy(true);
    setError(undefined);
    const out = await queueInOrder(qc, [
      { kind: KIND_READALONG, library_id: book.library_id, path: book.path, params: {} },
    ]);
    running.current = false;
    setBusy(false);
    // A generate or detect job queued since the dialog opened: say what it
    // waits for in the card's words, not the generic refusal's.
    const code = out.error instanceof ApiError ? out.error.code : undefined;
    const failed =
      code === 'generate_open' || code === 'detect_open'
        ? t(code === 'detect_open' ? 'book.readalong.afterDetect' : 'book.readalong.afterGenerate')
        : queueReport(out, 1, t, lang);
    if (failed !== undefined) {
      setError(failed);
      return;
    }
    const notes = skipNotes(out, t, lang, () => book.title || relBaseName(book.path)).join(' ');
    toast.add(
      out.queued
        ? {
            title: t('book.readalong.queued'),
            description: t('bridge.generate.queuedBody'),
            type: 'success',
          }
        : { title: t('bridge.queue.none'), description: notes },
    );
    onDone();
  };

  return (
    <form onSubmit={(ev) => void submit(ev)} className="contents" noValidate>
      <DialogBody className="flex flex-col gap-3 text-[13px]">
        {estimate.isError ? (
          <QueryError
            title={t('bridge.generate.estimateError')}
            error={estimate.error}
            onRetry={() => void estimate.refetch()}
          />
        ) : !e || !view ? (
          <div className="skel h-40 rounded-lg" role="status" aria-label={t('common.loading')} />
        ) : (
          <EstimateBody estimate={e} bridge={bridge} lang={lang} notWritable={view.notWritable} />
        )}
        <FormError>{error}</FormError>
      </DialogBody>
      <DialogFormFooter
        busy={busy}
        disabled={!canConfirm}
        // The button says what the title says: Make, or Make again (S265-CARD).
        submitLabel={remake ? t('book.readalong.remake') : t('book.readalong.submit')}
      />
    </form>
  );
}

/** The estimate's facts, and what warns or stops. */
function EstimateBody({
  estimate: e,
  bridge,
  lang,
  notWritable,
}: {
  estimate: ReadalongEstimate;
  bridge: ReadalongBridge;
  lang: string;
  notWritable: boolean;
}) {
  const { t } = useTranslation();
  const hours = e.figures && e.asr_hours > 0 ? e.asr_hours : null;
  return (
    <>
      {notWritable ? <NotWritableNotice /> : null}
      {/* An online Bridge too old to make read-alongs isn't offline (S265-DIALOG). */}
      {bridge === 'too_old' ? (
        <Notice tone="warn" icon={Unplug} title={t('book.readalong.tooOldTitle')}>
          {t('book.readalong.bridge.too_old', { version: READALONG_BRIDGE_VERSION })}
        </Notice>
      ) : bridge !== 'ready' ? (
        <OfflineNotice />
      ) : null}
      {!e.ebook ? (
        <p className="text-destructive" role="alert">
          {t('errors.noEbook')}
        </p>
      ) : null}
      <p>{t('book.readalong.body')}</p>
      <FactList
        rows={[
          [t('book.readalong.ebook'), e.ebook ? relBaseName(e.ebook.path) : undefined],
          [
            t('bridge.generate.asr'),
            hours === null ? (
              <NoEstimate />
            ) : (
              t('book.readalong.upTo', { what: formatHours(hours * 3600, lang) })
            ),
          ],
          [t('bridge.generate.cost'), t('health.detect.noAgent')],
        ]}
      />
      {e.readalong?.state === 'ready' && e.readalong.generated_at ? (
        <p className="text-[12.5px] text-muted-foreground">
          {t('book.readalong.replaces', { when: formatRelative(e.readalong.generated_at, lang) })}
        </p>
      ) : null}
      <p className="text-[12.5px] text-muted-foreground">{t('book.readalong.poorMatch')}</p>
    </>
  );
}
