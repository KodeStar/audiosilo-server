import { useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { ArrowRight, LoaderCircle, Lock } from 'lucide-react';
import { ApiError, api } from '@/api/client';
import { settleBookEdit } from '@/api/hooks';
import { OVERRIDE_FIELDS, type AdminBookDetail, type OverrideField } from '@/api/types';
import { ProvenanceMarker } from '@/components/provenance';
import { Button } from '@/components/ui/button';
import {
  Dialog,
  DialogBody,
  DialogClose,
  DialogContent,
  DialogFooter,
} from '@/components/ui/dialog';
import { toastError } from '@/lib/errors';
import { toast } from '@/lib/toast';
import { cn } from '@/lib/utils';
import { checkField, diffRows, saveRequest, type Drafts } from './book-model';

/**
 * The floating save bar (STYLEGUIDE.md "Save bar"): how many edits are waiting,
 * Discard, and the view's one pink button, Review and save.
 */
export function SaveBar({
  count,
  invalid,
  onDiscard,
  onReview,
}: {
  count: number;
  /** Fields whose draft the server would refuse: saving waits until they're fixed. */
  invalid: number;
  onDiscard: () => void;
  onReview: () => void;
}) {
  const { t } = useTranslation();
  return (
    <div className="float-bar" role="toolbar" aria-label={t('book.save.bar')}>
      <span className="px-1 text-[13.5px] tabular-nums" role="status">
        {t('book.save.unsaved', { count })}
        {invalid ? (
          <span className="opacity-75"> · {t('book.save.needsFix', { count: invalid })}</span>
        ) : null}
      </span>
      <Button
        variant="ghost"
        size="sm"
        className="text-inherit hover:bg-[color-mix(in_oklab,var(--primary-foreground)_12%,transparent)]"
        onClick={onDiscard}
      >
        {t('book.save.discard')}
      </Button>
      <Button variant="brand" size="sm" onClick={onReview} disabled={invalid > 0}>
        {t('book.save.review')}
      </Button>
    </div>
  );
}

/**
 * Save these changes? The diff (old struck through in red, new in green, the
 * source before -> Edited), then one PATCH for all of them.
 */
export function SaveDiffDialog({
  open,
  onOpenChange,
  detail,
  drafts,
  onSaved,
  onRefused,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  detail: AdminBookDetail;
  drafts: Drafts;
  onSaved: () => void;
  /** The server refused a field's value: say why under that field. */
  onRefused: (field: OverrideField, message: string) => void;
}) {
  const { t } = useTranslation();
  const count = Object.keys(drafts).length;
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        icon={Lock}
        title={t('book.diff.title')}
        description={t('book.diff.description')}
      >
        {open ? (
          <DiffBody
            detail={detail}
            drafts={drafts}
            count={count}
            onDone={() => onOpenChange(false)}
            onSaved={onSaved}
            onRefused={onRefused}
          />
        ) : null}
      </DialogContent>
    </Dialog>
  );
}

function DiffBody({
  detail,
  drafts,
  count,
  onDone,
  onSaved,
  onRefused,
}: {
  detail: AdminBookDetail;
  drafts: Drafts;
  count: number;
  onDone: () => void;
  onSaved: () => void;
  onRefused: (field: OverrideField, message: string) => void;
}) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [busy, setBusy] = useState(false);
  const b = detail.book;
  const rows = diffRows(drafts, detail.fields);

  const save = async () => {
    setBusy(true);
    try {
      const next = await api.editBook(b.library_id, b.path, saveRequest(drafts));
      settleBookEdit(qc, next);
      onSaved();
      onDone();
      toast.add({
        title: t('book.save.saved', { count, title: next.book.title }),
        description: t('book.save.savedBody'),
        type: 'success',
      });
    } catch (err) {
      setBusy(false);
      const field = err instanceof ApiError && err.code === 'invalid_override' ? err.field : '';
      if (field && (OVERRIDE_FIELDS as readonly string[]).includes(field)) {
        const f = field as OverrideField;
        // Our own wording when we know the rule; the server's sentence otherwise.
        const local = checkField(f, drafts[f] ?? '').error;
        onRefused(f, local ? t(local) : (err as ApiError).message);
        onDone();
      } else toastError(t('book.save.failed'), err);
    }
  };

  return (
    <>
      <DialogBody className="flex flex-col gap-2.5">
        {rows.map((r) => (
          <div key={r.field} className="rounded-[12px] border px-3.5 py-3">
            <div className="mb-2 flex flex-wrap items-center justify-between gap-2">
              <b className="text-[13px] font-semibold">{t(`book.field.${r.field}`)}</b>
              <span className="flex items-center gap-1.5">
                {r.source ? (
                  <ProvenanceMarker source={r.source} />
                ) : (
                  <span className="text-[11.5px] text-subtle-foreground">
                    {t('book.diff.unset')}
                  </span>
                )}
                <ArrowRight className="size-3.5 text-subtle-foreground" aria-hidden="true" />
                <ProvenanceMarker source="edited" />
              </span>
            </div>
            <DiffValue kind="before" value={r.before} />
            <DiffValue kind="after" value={r.after} />
          </div>
        ))}
      </DialogBody>
      <DialogFooter>
        <DialogClose render={<Button type="button" variant="ghost" />}>
          {t('book.diff.keepEditing')}
        </DialogClose>
        <Button onClick={() => void save()} disabled={busy}>
          {busy ? <LoaderCircle className="animate-spin" aria-hidden="true" /> : null}
          {t('book.diff.save', { count })}
        </Button>
      </DialogFooter>
    </>
  );
}

function DiffValue({ kind, value }: { kind: 'before' | 'after'; value: string }) {
  const { t } = useTranslation();
  const Tag = kind === 'before' ? 'del' : 'ins';
  return (
    <div
      className={cn(
        'rounded-[10px] bg-muted px-3 py-2 text-[13.5px] leading-[1.6] [overflow-wrap:anywhere]',
        kind === 'after' && 'mt-1.5',
      )}
    >
      <span className="sr-only">{t(`book.diff.${kind}`)} </span>
      <Tag
        className={cn(
          'rounded-[4px] px-0.5 whitespace-pre-line',
          kind === 'before'
            ? 'bg-[color-mix(in_oklab,var(--destructive)_15%,transparent)] text-destructive'
            : 'bg-[color-mix(in_oklab,var(--success)_18%,transparent)] text-success no-underline',
        )}
      >
        {value || t('book.diff.empty')}
      </Tag>
    </div>
  );
}
