import { useId } from 'react';
import { useTranslation } from 'react-i18next';
import { Check } from 'lucide-react';
import type { AdminLibrary, BookFacets, BoolFacet } from '@/api/types';
import { Button } from '@/components/ui/button';
import { Sheet } from '@/components/ui/sheet';
import { counted, formatNumber } from '@/lib/format';
import { cn } from '@/lib/utils';
import {
  ADDED,
  LENGTHS,
  PLAYBACK,
  YES_NO,
  type LibrarySearch,
  type Update,
} from '../library-search';
import { facetOptions, formatLabel, toggleValue, withoutFilters } from './books-model';
import { chipClass } from './chip-class';

interface Option {
  value: string;
  label: string;
  count?: number;
  on: boolean;
  toggle: () => void;
}

/**
 * The facet filters (STYLEGUIDE.md "Sheet"): one group of chips per dimension,
 * each with how many books it would match given the other filters (the facet
 * counts leave each dimension's own filter out). Repeatable groups (format,
 * codec) toggle values; the others hold one value, and pressing it again clears it.
 */
export function FilterSheet({
  open,
  onOpenChange,
  search,
  update,
  facets,
  libraries,
  metadataOn,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  search: LibrarySearch;
  update: Update;
  facets: BookFacets | undefined;
  libraries: AdminLibrary[];
  metadataOn: boolean;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const total = facets?.total ?? 0;

  const single = <K extends keyof LibrarySearch>(key: K, value: LibrarySearch[K]) => ({
    on: search[key] === value,
    toggle: () => update((prev) => ({ ...prev, [key]: prev[key] === value ? undefined : value })),
  });
  const multi = (key: 'format' | 'codec', value: string) => ({
    on: !!search[key]?.includes(value),
    toggle: () => update((prev) => ({ ...prev, [key]: toggleValue(prev[key], value) })),
  });
  const yesNo = (key: 'cover' | 'matched' | 'chapters' | 'edited', counts?: BoolFacet) =>
    YES_NO.map((v): Option => ({
      value: v,
      label: t(`books.value.${key}.${v}`),
      count: counts?.[v],
      ...single(key, v),
    }));

  const groups: { key: string; title: string; options: Option[] }[] = [
    {
      key: 'library',
      title: t('books.facet.library'),
      options: libraries.map((l) => ({
        value: String(l.id),
        label: l.name,
        count: facets?.libraries.find((c) => c.library_id === l.id)?.count ?? 0,
        ...single('library', l.id),
      })),
    },
    {
      key: 'format',
      title: t('books.facet.format'),
      options: facetOptions(facets?.formats, search.format).map((c) => ({
        value: c.value,
        label: formatLabel(c.value),
        count: c.count,
        ...multi('format', c.value),
      })),
    },
    {
      key: 'codec',
      title: t('books.facet.codec'),
      options: facetOptions(facets?.codecs, search.codec).map((c) => ({
        value: c.value,
        label: c.value,
        count: c.count,
        ...multi('codec', c.value),
      })),
    },
    {
      key: 'playback',
      title: t('books.facet.playback'),
      options: PLAYBACK.map((v) => ({
        value: v,
        label: t(`books.value.playback.${v}`),
        count: facets?.direct_playable[v === 'direct' ? 'yes' : 'no'],
        ...single('playback', v),
      })),
    },
    { key: 'cover', title: t('books.facet.cover'), options: yesNo('cover', facets?.has_cover) },
    ...(metadataOn
      ? [
          {
            key: 'matched',
            title: t('books.facet.matched'),
            options: yesNo('matched', facets?.matched),
          },
        ]
      : []),
    {
      key: 'chapters',
      title: t('books.facet.chapters'),
      options: yesNo('chapters', facets?.has_chapters),
    },
    { key: 'edited', title: t('books.facet.edited'), options: yesNo('edited', facets?.edited) },
    {
      key: 'length',
      title: t('books.facet.length'),
      options: LENGTHS.map((v) => ({
        value: v,
        label: t(`books.value.length.${v}`),
        ...single('length', v),
      })),
    },
    {
      key: 'added',
      title: t('books.facet.added'),
      options: ADDED.map((v) => ({
        value: v,
        label: t(`books.value.added.${v}`),
        ...single('added', v),
      })),
    },
  ];

  return (
    <Sheet
      open={open}
      onOpenChange={onOpenChange}
      title={t('books.sheet.title')}
      description={t('books.sheet.description')}
      footer={
        <>
          <Button variant="ghost" onClick={() => update((prev) => withoutFilters(prev, true))}>
            {t('books.sheet.clear')}
          </Button>
          <Button onClick={() => onOpenChange(false)}>
            {t('books.sheet.show', counted(total, lang))}
          </Button>
        </>
      }
    >
      {groups
        .filter((g) => g.options.length)
        .map((g) => (
          <FacetGroup key={g.key} title={g.title} options={g.options} lang={lang} />
        ))}
    </Sheet>
  );
}

function FacetGroup({ title, options, lang }: { title: string; options: Option[]; lang: string }) {
  const id = useId();
  return (
    <section aria-labelledby={id} className="border-b py-4 last:border-b-0">
      <h3
        id={id}
        className="mb-2.5 font-sans text-[12px] font-[650] tracking-[0.05em] text-muted-foreground uppercase"
      >
        {title}
      </h3>
      <div className="flex flex-wrap gap-1.5">
        {options.map((o) => (
          <FacetChip key={o.value} option={o} lang={lang} />
        ))}
      </div>
    </section>
  );
}

function FacetChip({ option: o, lang }: { option: Option; lang: string }) {
  return (
    <button
      type="button"
      aria-pressed={o.on}
      onClick={o.toggle}
      className={cn(
        chipClass,
        'hover:bg-accent aria-pressed:border-primary aria-pressed:bg-primary aria-pressed:text-primary-foreground',
      )}
    >
      {o.on ? <Check className="size-[13px]" strokeWidth={2.6} aria-hidden="true" /> : null}
      {o.label}
      {o.count !== undefined ? (
        <>
          {/* A real space: the accessible name is "M4B 3", not "M4B3". */}{' '}
          <span className="tabular-nums opacity-60">{formatNumber(o.count, lang)}</span>
        </>
      ) : null}
    </button>
  );
}
