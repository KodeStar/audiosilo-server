import { useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { Share2 } from 'lucide-react';
import { api } from '@/api/client';
import { keys, useShares } from '@/api/hooks';
import type { AdminBookDetail } from '@/api/types';
import { Dialog, DialogBody, DialogContent, DialogFormFooter } from '@/components/ui/dialog';
import { FormError } from '@/components/ui/field';
import { RadioCards, type RadioCardOption } from '@/components/ui/radio-cards';
import { errorMessage } from '@/lib/errors';
import { toast } from '@/lib/toast';

/** Adds the book's path to a share, so the share's members can listen to it. */
export function AddToShareDialog({
  open,
  onOpenChange,
  detail,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  detail: AdminBookDetail;
}) {
  const { t } = useTranslation();
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        icon={Share2}
        title={t('book.share.title')}
        description={t('book.share.description', { title: detail.book.title })}
      >
        {open ? <AddToShareForm detail={detail} onDone={() => onOpenChange(false)} /> : null}
      </DialogContent>
    </Dialog>
  );
}

function AddToShareForm({ detail, onDone }: { detail: AdminBookDetail; onDone: () => void }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const shares = useShares();
  const [choice, setChoice] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const b = detail.book;
  const already = new Set((detail.shares ?? []).map((s) => s.share_id));
  // Whole-library grants are access, not shares to add a book to.
  const options: RadioCardOption<string>[] = (shares.data ?? [])
    .filter((s) => s.whole_library_id === undefined)
    .map((s) => ({
      value: String(s.id),
      title: s.name,
      description: already.has(s.id)
        ? t('book.share.already')
        : t('book.share.members', { count: s.member_ids?.length ?? 0 }),
      disabled: already.has(s.id),
    }));

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    const share = shares.data?.find((s) => String(s.id) === choice);
    if (!share) return;
    setBusy(true);
    setError(undefined);
    try {
      await api.addSharePath(share.id, { library_id: b.library_id, path: b.path });
      void qc.invalidateQueries({ queryKey: keys.book(b.library_id, b.path) });
      void qc.invalidateQueries({ queryKey: keys.shares });
      toast.add({
        title: t('book.share.added', { share: share.name }),
        description: t('book.share.addedBody', { share: share.name, title: b.title }),
        type: 'success',
      });
      onDone();
    } catch (err) {
      setError(errorMessage(err, t));
      setBusy(false);
    }
  };

  return (
    <form onSubmit={(e) => void submit(e)} className="contents">
      <DialogBody className="flex flex-col gap-3">
        {shares.isPending ? (
          <div className="skel h-24" role="status" aria-label={t('common.loading')} />
        ) : options.length === 0 ? (
          <p className="text-muted-foreground">{t('book.share.noShares')}</p>
        ) : (
          <RadioCards
            label={t('book.share.title')}
            value={choice}
            onValueChange={setChoice}
            options={options}
          />
        )}
        <FormError>{error}</FormError>
      </DialogBody>
      <DialogFormFooter busy={busy} disabled={!choice} submitLabel={t('book.share.submit')} />
    </form>
  );
}
