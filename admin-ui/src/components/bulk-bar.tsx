import { useTranslation } from 'react-i18next';
import { X, type LucideIcon } from 'lucide-react';
import { Button } from '@/components/ui/button';
import { onInk } from '@/components/ui/on-ink';
import { counted } from '@/lib/format';

/**
 * The floating bulk action bar (STYLEGUIDE.md "Bulk action bar"): "N selected",
 * the actions (BulkAction children), and clear. Nothing shows with nothing
 * selected.
 */
export function BulkBar({
  count,
  label,
  onClear,
  children,
}: {
  count: number;
  /** The toolbar's accessible name. */
  label: string;
  onClear: () => void;
  children: React.ReactNode;
}) {
  const { t, i18n } = useTranslation();
  if (!count) return null;
  return (
    <div className="float-bar" role="toolbar" aria-label={label}>
      <span className="mr-2 font-bold whitespace-nowrap tabular-nums" aria-live="polite">
        {t('books.bulk.selected', counted(count, i18n.resolvedLanguage ?? 'en'))}
      </span>
      {children}
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

/** One action on the bulk bar. On a phone the label goes (the icon keeps it as its accessible name). */
export function BulkAction({
  icon: Icon,
  label,
  onClick,
}: {
  icon: LucideIcon;
  label: string;
  onClick: () => void;
}) {
  return (
    <Button variant="ghost" size="sm" className={onInk} onClick={onClick}>
      <Icon aria-hidden="true" />
      <span className="max-md:sr-only">{label}</span>
    </Button>
  );
}
