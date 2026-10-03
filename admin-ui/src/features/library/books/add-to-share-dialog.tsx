import { useState } from 'react';
import { Link } from '@tanstack/react-router';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { Share2 } from 'lucide-react';
import { api } from '@/api/client';
import { keys, useShares } from '@/api/hooks';
import type { AdminBook } from '@/api/types';
import { Button, buttonVariants } from '@/components/ui/button';
import { Dialog, DialogBody, DialogContent, DialogFormFooter } from '@/components/ui/dialog';
import { FormError } from '@/components/ui/field';
import { RadioCards } from '@/components/ui/radio-cards';
import { errorMessage } from '@/lib/errors';
import { formatNumber } from '@/lib/format';
import { toast } from '@/lib/toast';

/** Path rules added at once (one request each), so a large selection doesn't flood the server. */
const CONCURRENCY = 4;

/**
 * Adds the selected books to a named share: each book's path becomes one of the
 * share's rules, so everyone with the share sees it at once. Whole-library
 * grants aren't offered (they already hold every book of their library).
 */
export function AddToShareDialog({
  open,
  onOpenChange,
  books,
  onDone,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  books: AdminBook[];
  onDone: () => void;
}) {
  const { t, i18n } = useTranslation();
  const count = books.length;
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        icon={Share2}
        title={
          count === 1
            ? t('books.share.titleOne', { title: books[0]?.title })
            : t('books.share.title', {
                count,
                formatted: formatNumber(count, i18n.resolvedLanguage ?? 'en'),
              })
        }
        description={t('books.share.description')}
      >
        {open ? (
          <AddToShareForm
            books={books}
            onClose={() => onOpenChange(false)}
            onDone={() => {
              onOpenChange(false);
              onDone();
            }}
          />
        ) : null}
      </DialogContent>
    </Dialog>
  );
}

function AddToShareForm({
  books,
  onClose,
  onDone,
}: {
  books: AdminBook[];
  onClose: () => void;
  onDone: () => void;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const qc = useQueryClient();
  const shares = useShares();
  const named = (shares.data ?? []).filter((s) => s.whole_library_id === undefined);
  const [picked, setPicked] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const share = named.find((s) => String(s.id) === picked) ?? named[0];

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!share || busy) return;
    setBusy(true);
    setError('');
    try {
      for (let i = 0; i < books.length; i += CONCURRENCY) {
        await Promise.all(
          books
            .slice(i, i + CONCURRENCY)
            .map((b) => api.addSharePath(share.id, { library_id: b.library_id, path: b.path })),
        );
      }
      toast.add({
        title: t('books.share.done', { name: share.name }),
        description: t('books.share.doneBody', {
          count: books.length,
          formatted: formatNumber(books.length, lang),
        }),
        type: 'success',
      });
      onDone();
    } catch (err) {
      setError(errorMessage(err, t));
      setBusy(false);
    } finally {
      // Rules added before a failure are kept: the share and book pages refetch either way.
      void qc.invalidateQueries({ queryKey: keys.shares });
      for (const b of books)
        void qc.invalidateQueries({ queryKey: keys.book(b.library_id, b.path) });
    }
  };

  if (shares.data && !named.length) {
    return (
      <DialogBody className="flex flex-col items-start gap-3">
        <p className="font-semibold">{t('books.share.none.title')}</p>
        <p className="text-muted-foreground">{t('books.share.none.body')}</p>
        <div className="flex gap-2">
          <Link
            to="/people/{-$section}"
            params={{ section: 'shares' }}
            className={buttonVariants({ variant: 'outline', size: 'sm' })}
            onClick={onClose}
          >
            {t('books.share.none.action')}
          </Link>
          <Button variant="ghost" size="sm" onClick={onClose}>
            {t('common.cancel')}
          </Button>
        </div>
      </DialogBody>
    );
  }

  return (
    <form onSubmit={(e) => void submit(e)} noValidate className="contents">
      <DialogBody className="flex flex-col gap-3">
        {shares.isError ? (
          <FormError>{errorMessage(shares.error, t)}</FormError>
        ) : !shares.data ? (
          <div
            className="skel h-[120px] rounded-xl"
            role="status"
            aria-label={t('common.loading')}
          />
        ) : (
          <RadioCards
            label={t('books.share.pick')}
            value={share ? String(share.id) : undefined}
            onValueChange={setPicked}
            options={named.map((s) => ({
              value: String(s.id),
              title: s.name,
              description: [
                t('books.share.members', { count: s.member_ids.length }),
                t('books.share.rules', { count: s.paths?.length ?? 0 }),
              ].join(' · '),
            }))}
          />
        )}
        <FormError>{error ? t('books.share.failed', { reason: error }) : null}</FormError>
      </DialogBody>
      <DialogFormFooter busy={busy} disabled={!share} submitLabel={t('books.share.submit')} />
    </form>
  );
}
