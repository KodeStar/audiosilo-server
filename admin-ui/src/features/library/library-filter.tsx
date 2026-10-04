import { useTranslation } from 'react-i18next';
import { useLibraries } from '@/api/hooks';
import type { BookFacets } from '@/api/types';
import { NativeSelect } from '@/components/ui/native-select';
import { SegmentedControl } from '@/components/ui/segmented-control';
import { formatNumber } from '@/lib/format';
import { cn } from '@/lib/utils';
import { useLibraryParam } from './library-param';

/** Up to this many libraries pick from a segmented control; more from a select. */
const SEGMENTED_MAX = 4;

/**
 * One library, or all of them (Books, Authors, Narrators, Series, Folders): a
 * segmented control, a select once there are many, hidden with a single
 * library, where it would filter nothing. With `counts` (the facets', which
 * ignore the library filter) each choice shows its books; an unreachable
 * library reads "offline" instead. It drives `?library=` unless given a
 * `value` and `onChange` of its own.
 */
export function LibraryFilter({
  counts,
  allowAll = true,
  value,
  onChange,
  className,
}: {
  counts?: BookFacets['libraries'];
  /** "All" is a choice (Folders shows one library at a time). */
  allowAll?: boolean;
  value?: number;
  onChange?: (id: number | undefined) => void;
  className?: string;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const libraries = useLibraries().data ?? [];
  const [param, setParam] = useLibraryParam();
  if (libraries.length < 2) return null;

  const change = (id: number) => (onChange ?? setParam)(id || undefined);
  const countOf = (id: number) => counts?.find((c) => c.library_id === id)?.count ?? 0;
  const all = counts?.reduce((n, c) => n + c.count, 0);
  const options = [
    ...(allowAll
      ? [
          {
            value: 0,
            name: t('library-filter.all'),
            aside: all !== undefined ? formatNumber(all, lang) : undefined,
          },
        ]
      : []),
    ...libraries.map((l) => ({
      value: l.id,
      name: l.name,
      aside: !l.available
        ? t('library-filter.offline')
        : counts
          ? formatNumber(countOf(l.id), lang)
          : undefined,
    })),
  ];
  const selected = value ?? param ?? 0;
  const label = t('library-filter.label');

  if (libraries.length > SEGMENTED_MAX) {
    return (
      <NativeSelect
        aria-label={label}
        className={cn('w-[220px]', className)}
        value={selected}
        onChange={(e) => change(Number(e.target.value))}
      >
        {options.map((o) => (
          <option key={o.value} value={o.value}>
            {o.aside !== undefined ? `${o.name} · ${o.aside}` : o.name}
          </option>
        ))}
      </NativeSelect>
    );
  }
  return (
    <div className={cn('hscroll max-w-full', className)}>
      <SegmentedControl
        label={label}
        value={selected}
        onChange={change}
        options={options.map((o) => ({
          value: o.value,
          label: (
            <>
              {o.name}
              {o.aside !== undefined ? (
                <>
                  {/* A real space: the accessible name is "Fiction 3", not "Fiction3". */}{' '}
                  <span className="ml-1.5 text-subtle-foreground tabular-nums">{o.aside}</span>
                </>
              ) : null}
            </>
          ),
        }))}
      />
    </div>
  );
}
