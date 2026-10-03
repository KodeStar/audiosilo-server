import { useEffect, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { LayoutGrid, List, Search, SlidersHorizontal, X } from 'lucide-react';
import type { AdminBookSort, AdminLibrary } from '@/api/types';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { NativeSelect } from '@/components/ui/native-select';
import { SegmentedControl } from '@/components/ui/segmented-control';
import { cn } from '@/lib/utils';
import type { LibrarySearch } from '../library-search';
import {
  SORT_OPTIONS,
  activeFilters,
  sheetFilterCount,
  withoutFilter,
  withoutFilters,
} from './books-model';
import { chipClass } from './chip-class';
import { chipLabel } from './filter-labels';

type Update = (fn: (prev: LibrarySearch) => LibrarySearch) => void;

/** How long the search box waits for typing to pause before it filters. */
const SEARCH_DEBOUNCE_MS = 250;

/**
 * The sticky row over the list: its heading, the search box, the Filters button
 * (with how many are on), the sort menu and the grid/table switch.
 */
export function BooksToolbar({
  heading,
  search,
  update,
  onOpenFilters,
}: {
  heading: string;
  search: LibrarySearch;
  update: Update;
  onOpenFilters: () => void;
}) {
  const { t } = useTranslation();
  const count = sheetFilterCount(search);
  const sort = search.sort ?? 'title';

  return (
    <div className="sticky top-[104px] z-10 -mx-1 mb-3 flex flex-wrap items-center justify-between gap-2.5 bg-background px-1 py-2.5 md:top-[114px]">
      <h2 className="h2 max-md:sr-only" aria-live="polite">
        {heading}
      </h2>
      <div className="flex flex-1 flex-wrap items-center justify-end gap-2">
        <SearchBox
          value={search.q ?? ''}
          onChange={(q) => update((prev) => ({ ...prev, q: q || undefined }))}
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
        <NativeSelect
          className="w-auto"
          aria-label={t('books.sort.label')}
          value={sort}
          onChange={(e) => {
            const next = e.target.value as AdminBookSort;
            update((prev) => ({
              ...prev,
              sort: next === 'title' ? undefined : next,
              order: undefined,
            }));
          }}
        >
          {SORT_OPTIONS.map((s) => (
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

/**
 * The search box: types freely, filters once typing pauses. A change from
 * outside (a chip's clear, Back) replaces what is typed.
 */
function SearchBox({ value, onChange }: { value: string; onChange: (q: string) => void }) {
  const { t } = useTranslation();
  const [text, setText] = useState(value);
  // The last value this box sent (or was given), to tell outside changes apart.
  const sent = useRef(value);
  const latest = useRef(onChange);
  useEffect(() => {
    latest.current = onChange;
  });

  useEffect(() => {
    if (value === sent.current) return;
    sent.current = value;
    setText(value);
  }, [value]);

  useEffect(() => {
    if (text.trim() === sent.current.trim()) return;
    const id = setTimeout(() => {
      sent.current = text.trim();
      latest.current(text.trim());
    }, SEARCH_DEBOUNCE_MS);
    return () => clearTimeout(id);
  }, [text]);

  return (
    <label className="relative flex min-w-[180px] flex-[0_1_300px] items-center">
      <Search
        className="pointer-events-none absolute left-3 size-4 text-muted-foreground"
        aria-hidden="true"
      />
      <Input
        value={text}
        onChange={(e) => setText(e.target.value)}
        aria-label={t('books.search.label')}
        placeholder={t('books.search.placeholder')}
        autoComplete="off"
        className="pr-9 pl-9"
      />
      {text ? (
        <button
          type="button"
          aria-label={t('books.search.clear')}
          onClick={() => {
            setText('');
            sent.current = '';
            latest.current('');
          }}
          className="absolute right-1.5 grid size-7 place-items-center rounded-sm text-muted-foreground hover:bg-accent hover:text-foreground"
        >
          <X className="size-4" aria-hidden="true" />
        </button>
      ) : null}
    </label>
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
