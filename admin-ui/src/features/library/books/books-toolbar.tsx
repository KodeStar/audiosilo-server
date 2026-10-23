import { useTranslation } from 'react-i18next';
import { LayoutGrid, List, ListChecks, SlidersHorizontal, X } from 'lucide-react';
import { ADMIN_BOOK_SORTS, type AdminBookSort, type AdminLibrary } from '@/api/types';
import { SearchBox } from '@/components/search-box';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { NativeSelect } from '@/components/ui/native-select';
import { SegmentedControl } from '@/components/ui/segmented-control';
import { cn } from '@/lib/utils';
import type { LibrarySearch, Update } from '../library-search';
import {
  activeFilters,
  defaultSort,
  sheetFilterCount,
  sortOf,
  withoutFilter,
  withoutFilters,
} from './books-model';
import { chipClass } from './chip-class';
import { chipLabel } from './filter-labels';

/**
 * The sticky row over the list: its heading, the search box, the Filters button
 * (with how many are on), Select (picking books without a first click on one),
 * the sort menu and the grid/table switch.
 */
export function BooksToolbar({
  heading,
  search,
  update,
  onOpenFilters,
  selecting,
  onToggleSelect,
}: {
  heading: string;
  search: LibrarySearch;
  update: Update;
  onOpenFilters: () => void;
  /** Whether books are being picked (the Select toggle, or a selection). */
  selecting?: boolean;
  /** Select starts picking without a first book; Done ends it and clears the selection. */
  onToggleSelect?: () => void;
}) {
  const { t } = useTranslation();
  const count = sheetFilterCount(search);
  const sort = sortOf(search);

  return (
    // Sticks under the sticky header: both its rows, and their hairlines.
    <div className="sticky top-[calc(var(--topbar-h)+var(--subbar-h)+2px)] z-10 -mx-1 mb-3 flex flex-wrap items-center justify-between gap-2.5 bg-background px-1 py-2.5">
      <h2 className="h2 max-md:sr-only" aria-live="polite">
        {heading}
      </h2>
      <div className="flex flex-1 flex-wrap items-center justify-end gap-2">
        <SearchBox
          value={search.q ?? ''}
          onChange={(q) => update((prev) => ({ ...prev, q: q || undefined }))}
          label={t('books.search.label')}
          placeholder={t('books.search.placeholder')}
          clearLabel={t('books.search.clear')}
          className="min-w-[180px] flex-[0_1_300px]"
        />
        <Button variant="outline" onClick={onOpenFilters}>
          <SlidersHorizontal aria-hidden="true" />
          {t('books.filters')}
          {count ? (
            <Badge variant="brand" className="h-[18px] tabular-nums">
              {count}
            </Badge>
          ) : null}
        </Button>
        {onToggleSelect ? (
          <Button variant={selecting ? 'secondary' : 'outline'} onClick={onToggleSelect}>
            {selecting ? (
              t('books.select.done')
            ) : (
              <>
                <ListChecks aria-hidden="true" />
                {t('books.select.toggle')}
              </>
            )}
          </Button>
        ) : null}
        <NativeSelect
          className="w-auto"
          aria-label={t('books.sort.label')}
          value={sort}
          onChange={(e) => {
            const next = e.target.value as AdminBookSort;
            // A chosen sort is kept, so it survives a filter changing the default;
            // only the plain list's own default (title) is left out of the URL.
            update((prev) => ({
              ...prev,
              sort: next === 'title' && defaultSort(prev) === 'title' ? undefined : next,
              order: undefined,
            }));
          }}
        >
          {ADMIN_BOOK_SORTS.map((s) => (
            <option key={s} value={s}>
              {t(`books.sort.${s}`)}
            </option>
          ))}
        </NativeSelect>
        <SegmentedControl
          label={t('books.view.label')}
          value={search.view ?? 'grid'}
          onChange={(v) =>
            update((prev) => ({ ...prev, view: v === 'table' ? 'table' : undefined }))
          }
          options={[
            {
              value: 'grid',
              label: (
                <>
                  <LayoutGrid className="size-4" aria-hidden="true" />
                  <span className="sr-only">{t('books.view.grid')}</span>
                </>
              ),
            },
            {
              value: 'table',
              label: (
                <>
                  <List className="size-4" aria-hidden="true" />
                  <span className="sr-only">{t('books.view.table')}</span>
                </>
              ),
            },
          ]}
        />
      </div>
    </div>
  );
}

/** One removable chip per active filter and the search, plus "Clear all". */
export function ActiveChips({
  search,
  update,
  libraries,
}: {
  search: LibrarySearch;
  update: Update;
  libraries: AdminLibrary[];
}) {
  const { t } = useTranslation();
  const filters = activeFilters(search);
  const q = search.q?.trim();
  if (!filters.length && !q) return null;

  const chip = (key: string, label: string, removeLabel: string, remove: () => void) => (
    <li key={key}>
      <span
        className={cn(
          chipClass,
          'border-[color-mix(in_oklab,var(--brand)_30%,transparent)] bg-brand-soft pr-1.5 text-brand-ink',
        )}
      >
        <span className="max-w-[260px] truncate">{label}</span>
        <button
          type="button"
          aria-label={removeLabel}
          onClick={remove}
          className="grid size-[18px] place-items-center rounded-full opacity-70 hover:bg-[color-mix(in_oklab,currentColor_15%,transparent)] hover:opacity-100"
        >
          <X className="size-3" aria-hidden="true" />
        </button>
      </span>
    </li>
  );

  return (
    <ul className="mb-5 flex flex-wrap items-center gap-1.5" aria-label={t('books.chips.label')}>
      {q
        ? chip('q', t('books.chip.search', { q }), t('books.search.clear'), () =>
            update((prev) => ({ ...prev, q: undefined })),
          )
        : null}
      {filters.map((f) => {
        const label = chipLabel(t, f, libraries);
        return chip(`${f.key}:${f.value}`, label, t('books.chip.remove', { label }), () =>
          update((prev) => withoutFilter(prev, f)),
        );
      })}
      <li>
        <Button variant="link" size="sm" onClick={() => update((prev) => withoutFilters(prev))}>
          {t('books.chip.clearAll')}
        </Button>
      </li>
    </ul>
  );
}
