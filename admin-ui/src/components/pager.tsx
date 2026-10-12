import { useTranslation } from 'react-i18next';
import { ChevronLeft, ChevronRight } from 'lucide-react';
import { Button } from '@/components/ui/button';
import { formatNumber } from '@/lib/format';
import { pagerItems } from '@/lib/pager';
import { cn } from '@/lib/utils';

/**
 * Numbered pages for a long list: previous, the first and last pages and the ones
 * around the current page (skipped runs as an ellipsis), next. Nothing for a
 * single page. The caller keeps the page (usually a `page` search param, so Back
 * returns to it) and is told which page was picked.
 */
export function Pager({
  page,
  count,
  onPage,
  className,
}: {
  /** The current page, from 1. */
  page: number;
  /** How many pages there are. */
  count: number;
  onPage: (page: number) => void;
  className?: string;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const items = pagerItems(page, count);
  if (items.length === 0) return null;
  const fmt = (n: number) => formatNumber(n, lang);
  return (
    <nav aria-label={t('pager.label')} className={cn('flex justify-center', className)}>
      <ul className="flex flex-wrap items-center justify-center gap-1">
        <li>
          <Button
            variant="ghost"
            size="sm"
            disabled={page <= 1}
            onClick={() => onPage(page - 1)}
            aria-label={t('pager.previous')}
          >
            <ChevronLeft aria-hidden="true" />
            <span className="hidden sm:inline">{t('pager.previous')}</span>
          </Button>
        </li>
        {items.map((item, i) =>
          item === 'gap' ? (
            <li key={`gap-${i}`} aria-hidden="true" className="px-1 text-subtle-foreground">
              …
            </li>
          ) : (
            <li key={item}>
              <Button
                variant={item === page ? 'outline' : 'ghost'}
                size="sm"
                className="min-w-[30px] tabular-nums"
                aria-current={item === page ? 'page' : undefined}
                aria-label={t('pager.page', { page: fmt(item) })}
                onClick={() => item !== page && onPage(item)}
              >
                {fmt(item)}
              </Button>
            </li>
          ),
        )}
        <li>
          <Button
            variant="ghost"
            size="sm"
            disabled={page >= count}
            onClick={() => onPage(page + 1)}
            aria-label={t('pager.next')}
          >
            <span className="hidden sm:inline">{t('pager.next')}</span>
            <ChevronRight aria-hidden="true" />
          </Button>
        </li>
      </ul>
    </nav>
  );
}
