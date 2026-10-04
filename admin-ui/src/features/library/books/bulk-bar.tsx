import { useTranslation } from 'react-i18next';
import { Pencil, Share2, X } from 'lucide-react';
import { Button } from '@/components/ui/button';
import { onInk } from '@/components/ui/on-ink';
import { counted } from '@/lib/format';

/**
 * The floating bulk action bar (STYLEGUIDE.md "Bulk action bar"): "N selected",
 * the actions, and clear. On a phone the labels go (the icons keep them as their
 * accessible names).
 */
export function BulkBar({
  count,
  onEdit,
  onShare,
  onClear,
}: {
  count: number;
  onEdit: () => void;
  onShare: () => void;
  onClear: () => void;
}) {
  const { t, i18n } = useTranslation();
  if (!count) return null;
  return (
    <div className="float-bar" role="toolbar" aria-label={t('books.bulk.label')}>
      <span className="mr-2 font-bold whitespace-nowrap tabular-nums" aria-live="polite">
        {t('books.bulk.selected', counted(count, i18n.resolvedLanguage ?? 'en'))}
      </span>
      <Button variant="ghost" size="sm" className={onInk} onClick={onEdit}>
        <Pencil aria-hidden="true" />
        <span className="max-md:sr-only">{t('books.bulk.edit')}</span>
      </Button>
      <Button variant="ghost" size="sm" className={onInk} onClick={onShare}>
        <Share2 aria-hidden="true" />
        <span className="max-md:sr-only">{t('books.bulk.share')}</span>
      </Button>
      <span
        className="mx-1 h-[22px] w-px bg-[color-mix(in_oklab,var(--primary-foreground)_20%,transparent)]"
        aria-hidden="true"
      />
      <Button
        variant="ghost"
        size="icon-sm"
        className={onInk}
        aria-label={t('books.bulk.clear')}
        onClick={onClear}
      >
        <X aria-hidden="true" />
      </Button>
    </div>
  );
}
