import { useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { ChevronRight, ScanSearch } from 'lucide-react';
import type { BridgeJobRequest, BridgeLibrary } from '@/api/types';
import { Dialog, DialogBody, DialogContent, DialogFormFooter } from '@/components/ui/dialog';
import { Field } from '@/components/ui/field';
import { Input } from '@/components/ui/input';
import { describedBy } from '@/lib/a11y';
import { formatNumber } from '@/lib/format';
import { queueJob } from './bridge-actions';
import { holdParams, KIND_VERIFY, MAX_HOLD_SECONDS } from './bridge-model';

/**
 * Verify with options: one book instead of the library's root, and (behind
 * More options) a hold, which keeps the job running so cancelling it or
 * stopping the Bridge mid-job can be tried.
 */
export function VerifyDialog({
  library,
  online,
  onClose,
}: {
  library: BridgeLibrary | null;
  online: boolean;
  onClose: () => void;
}) {
  const { t } = useTranslation();
  return (
    <Dialog open={library !== null} onOpenChange={(o) => !o && onClose()}>
      <DialogContent
        title={library ? t('bridge.verify.title', { name: library.name }) : ''}
        description={t('bridge.verify.description')}
        icon={ScanSearch}
        tone="info"
      >
        {library ? <VerifyForm library={library} online={online} onDone={onClose} /> : null}
      </DialogContent>
    </Dialog>
  );
}

function VerifyForm({
  library,
  online,
  onDone,
}: {
  library: BridgeLibrary;
  online: boolean;
  onDone: () => void;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const qc = useQueryClient();
  const [path, setPath] = useState('');
  const [hold, setHold] = useState('0');
  const [busy, setBusy] = useState(false);
  const params = holdParams(hold);
  const holdError =
    params === null
      ? t('bridge.verify.holdInvalid', { max: formatNumber(MAX_HOLD_SECONDS, lang) })
      : undefined;

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (params === null || busy) return;
    const req: BridgeJobRequest = { kind: KIND_VERIFY, library_id: library.id };
    if (path.trim()) req.path = path.trim();
    if (params) req.params = params;
    setBusy(true);
    const ok = await queueJob(qc, req, { name: library.name, online });
    setBusy(false);
    if (ok) onDone();
  };

  return (
    <form onSubmit={(e) => void submit(e)} className="contents" noValidate>
      <DialogBody className="flex flex-col gap-4">
        <Field
          htmlFor="verify-path"
          label={t('bridge.verify.path')}
          description={t('bridge.verify.pathBody')}
        >
          <Input
            id="verify-path"
            value={path}
            onChange={(e) => setPath(e.target.value)}
            placeholder={t('bridge.verify.pathPlaceholder')}
            className="font-mono text-[12.5px]"
            autoComplete="off"
            spellCheck={false}
            aria-describedby="verify-path-desc"
          />
        </Field>
        <details className="group rounded-lg border">
          <summary className="flex cursor-pointer list-none items-center gap-1.5 rounded-lg px-4 py-2.5 font-semibold select-none [&::-webkit-details-marker]:hidden">
            <ChevronRight
              className="size-4 text-muted-foreground transition-transform duration-(--dur-1) group-open:rotate-90"
              aria-hidden="true"
            />
            {t('bridge.verify.more')}
          </summary>
          <div className="border-t px-4 py-3">
            <Field
              htmlFor="verify-hold"
              label={t('bridge.verify.hold')}
              description={t('bridge.verify.holdBody')}
              error={holdError}
            >
              <Input
                id="verify-hold"
                type="number"
                inputMode="numeric"
                min={0}
                max={MAX_HOLD_SECONDS}
                value={hold}
                onChange={(e) => setHold(e.target.value)}
                className="w-32 tabular-nums"
                aria-invalid={holdError ? true : undefined}
                aria-describedby={describedBy('verify-hold', Boolean(holdError), true)}
              />
            </Field>
          </div>
        </details>
      </DialogBody>
      <DialogFormFooter
        busy={busy}
        disabled={params === null}
        submitLabel={t('bridge.verify.submit')}
      />
    </form>
  );
}
