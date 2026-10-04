import { useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { Pencil, TriangleAlert } from 'lucide-react';
import { BULK_LIMIT, api } from '@/api/client';
import { invalidateBooks } from '@/api/hooks';
import type { AdminBook } from '@/api/types';
import { BookCover } from '@/components/book-cover';
import { Notice } from '@/components/notice';
import { Dialog, DialogBody, DialogContent, DialogFormFooter } from '@/components/ui/dialog';
import { FormError } from '@/components/ui/field';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import { refKey, refOf } from '@/lib/book-route';
import { errorMessage } from '@/lib/errors';
import { counted, formatNumber } from '@/lib/format';
import { toast } from '@/lib/toast';
import { BULK_FIELDS, bulkSet, commonValue, type BulkField } from './books-model';

/** How many covers the dialog fans out before "+N". */
const FAN = 8;

/**
 * Edit fields over the selection: author, narrator, series. Only filled fields
 * change; each change is saved as an edit (locked against rescans), all or
 * nothing, and the files are never touched.
 */
export function BulkEditDialog({
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
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        icon={Pencil}
        title={t('books.edit.title', counted(books.length, i18n.resolvedLanguage ?? 'en'))}
        description={t('books.edit.description')}
      >
        {open ? (
          <BulkEditForm
            books={books}
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

function BulkEditForm({ books, onDone }: { books: AdminBook[]; onDone: () => void }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const qc = useQueryClient();
  const [values, setValues] = useState<Partial<Record<BulkField, string>>>({});
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const set = bulkSet(values);
  const changes = Object.keys(set).length;
  const tooMany = books.length > BULK_LIMIT;
  const count = books.length;

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!changes || tooMany || busy) return;
    setBusy(true);
    setError('');
    try {
      const refs = books.map(refOf);
      const res = await api.bulkEdit(refs, { set });
      invalidateBooks(qc, refs);
      toast.add({
        title: t('books.edit.done', counted(res.updated, lang)),
        description: t('books.edit.doneBody'),
        type: 'success',
      });
      onDone();
    } catch (err) {
      setError(errorMessage(err, t));
      setBusy(false);
    }
  };

  const placeholder = (f: BulkField) => {
    const c = commonValue(books, f);
    if (c.same) return c.value || t('books.edit.empty');
    return t('books.edit.mixed', { count: c.distinct });
  };

  return (
    <form onSubmit={(e) => void submit(e)} noValidate className="contents">
      <DialogBody className="flex flex-col gap-4">
        <div className="flex items-center" aria-hidden="true">
          {books.slice(0, FAN).map((b, i) => (
            <span
              key={refKey(b)}
              className="w-11 shrink-0"
              style={{
                marginLeft: i ? -12 : 0,
                transform: `rotate(${((i % 3) - 1) * 3}deg)`,
              }}
            >
              <BookCover
                libraryId={b.library_id}
                path={b.path}
                title={b.title}
                author={b.author}
                size={160}
              />
            </span>
          ))}
          {count > FAN ? (
            <span className="ml-2.5 text-muted-foreground tabular-nums">
              {t('books.edit.more', { formatted: formatNumber(count - FAN, lang) })}
            </span>
          ) : null}
        </div>
        {tooMany ? (
          <Notice
            tone="warn"
            icon={TriangleAlert}
            title={t('books.edit.tooMany', { max: formatNumber(BULK_LIMIT, lang) })}
          >
            {t('books.edit.tooManyBody', { max: formatNumber(BULK_LIMIT, lang) })}
          </Notice>
        ) : null}
        {BULK_FIELDS.map((f) => (
          <div key={f} className="flex flex-col gap-1.5">
            <Label htmlFor={`bulk-${f}`}>{t(`books.edit.field.${f}`)}</Label>
            <Input
              id={`bulk-${f}`}
              autoComplete="off"
              value={values[f] ?? ''}
              placeholder={placeholder(f)}
              disabled={tooMany}
              onChange={(e) => setValues((v) => ({ ...v, [f]: e.target.value }))}
            />
          </div>
        ))}
        <FormError>{error ? t('books.edit.failed', { reason: error }) : null}</FormError>
      </DialogBody>
      <DialogFormFooter
        busy={busy}
        disabled={!changes || tooMany}
        submitLabel={t('books.edit.apply', counted(count, lang))}
      />
    </form>
  );
}
