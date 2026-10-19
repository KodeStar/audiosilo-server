import { useTranslation } from 'react-i18next';
import { ChevronDown, ChevronUp } from 'lucide-react';
import { Button } from '@/components/ui/button';

/** Rows a book page list shows before "Show all". */
export const FIRST_ROWS = 10;

/**
 * "Show all N" / "Show fewer" under a book page list cut at FIRST_ROWS; nothing
 * when the list is no longer than that.
 */
export function ShowAllToggle({
  all,
  total,
  label,
  onToggle,
  className,
}: {
  all: boolean;
  total: number;
  /** The "Show all N" label. */
  label: string;
  onToggle: () => void;
  className?: string;
}) {
  const { t } = useTranslation();
  if (total <= FIRST_ROWS) return null;
  return (
    <div className={className}>
      <Button variant="ghost" size="sm" onClick={onToggle}>
        {all ? t('book.chapters.fewer') : label}
        {all ? <ChevronUp aria-hidden="true" /> : <ChevronDown aria-hidden="true" />}
      </Button>
    </div>
  );
}
