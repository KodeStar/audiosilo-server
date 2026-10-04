import { useState } from 'react';
import { Link } from '@tanstack/react-router';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { Share2 } from 'lucide-react';
import { SHARE_RULES_LIMIT, api } from '@/api/client';
import { invalidateBookPages, keys, useShares } from '@/api/hooks';
import type { BookRef } from '@/api/types';
import { Button, buttonVariants } from '@/components/ui/button';
import { Dialog, DialogBody, DialogContent, DialogFormFooter } from '@/components/ui/dialog';
import { FormError } from '@/components/ui/field';
import { RadioCards } from '@/components/ui/radio-cards';
import { refOf } from '@/lib/book-route';
import { errorMessage } from '@/lib/errors';
import { counted } from '@/lib/format';
import { toast } from '@/lib/toast';
import { chunk } from '@/lib/utils';

/**
 * Adds books to a named share (the Books selection, or the book page's one
 * book): each book's path becomes one of the share's rules, so everyone with
 * the share sees it at once. Whole-library grants aren't offered (they already
 * hold every book of their library), and shares in `alreadyIn` (by id) show,
 * disabled, as already holding the book.
 */
export function AddToShareDialog({
  open,
  onOpenChange,
  books,
  alreadyIn,
  onDone,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** The books, with their titles when known (one book's title names the dialog). */
  books: (BookRef & { title?: string })[];
  alreadyIn?: ReadonlySet<number>;
  onDone?: () => void;
}) {
  const { t, i18n } = useTranslation();
  const one = books.length === 1 ? books[0].title : undefined;
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        icon={Share2}
        title={
          one
            ? t('books.share.titleOne', { title: one })
            : t('books.share.title', counted(books.length, i18n.resolvedLanguage ?? 'en'))
        }
        description={t('books.share.description')}
      >
        {open ? (
          <AddToShareForm
            books={books}
            alreadyIn={alreadyIn}
            onClose={() => onOpenChange(false)}
            onDone={() => {
              onOpenChange(false);
              onDone?.();
            }}
          />
        ) : null}
      </DialogContent>
    </Dialog>
  );
}

function AddToShareForm({
  books,
  alreadyIn,
  onClose,
  onDone,
}: {
  books: BookRef[];
  alreadyIn?: ReadonlySet<number>;
  onClose: () => void;
  onDone: () => void;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const qc = useQueryClient();
  const shares = useShares();
  const named = (shares.data ?? []).filter((s) => s.whole_library_id === undefined);
  const open = named.filter((s) => !alreadyIn?.has(s.id));
  const [picked, setPicked] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const share = open.find((s) => String(s.id) === picked) ?? open[0];

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!share || busy) return;
    setBusy(true);
    setError('');
    try {
      // One request per SHARE_RULES_LIMIT books, each all or nothing.
      for (const rules of chunk(books.map(refOf), SHARE_RULES_LIMIT)) {
        await api.addSharePaths(share.id, rules);
      }
      toast.add({
        title: t('books.share.done', { name: share.name }),
        description: t('books.share.doneBody', counted(books.length, lang)),
        type: 'success',
      });
      onDone();
    } catch (err) {
      setError(errorMessage(err, t));
      setBusy(false);
    } finally {
      // Rules added before a failure are kept: the share and book pages refetch either way.
      void qc.invalidateQueries({ queryKey: keys.shares });
      invalidateBookPages(qc, books);
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
              description: alreadyIn?.has(s.id)
                ? t('books.share.already')
                : [
                    t('books.share.members', { count: s.member_ids.length }),
                    t('books.share.rules', { count: s.paths?.length ?? 0 }),
                  ].join(' · '),
              disabled: alreadyIn?.has(s.id),
            }))}
          />
        )}
        <FormError>{error ? t('books.share.failed', { reason: error }) : null}</FormError>
      </DialogBody>
      <DialogFormFooter busy={busy} disabled={!share} submitLabel={t('books.share.submit')} />
    </form>
  );
}
