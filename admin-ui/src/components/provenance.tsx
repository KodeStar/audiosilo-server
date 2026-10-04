import { useTranslation } from 'react-i18next';
import { FolderOpen, Globe, Lock, Tag, type LucideIcon } from 'lucide-react';
import type { FieldSource } from '@/api/types';
import { cn } from '@/lib/utils';

// Provenance marker (STYLEGUIDE.md "Provenance marker"): where a field's value
// came from, as a 20px soft pill with the fixed icon for each source. `short`
// keeps only the icon (tables, chapter rows); the label is then its accessible
// name. "Unsaved" is the dashed pink outline of a field edited but not saved.

const SOURCES: Record<Exclude<FieldSource, ''>, { icon: LucideIcon; className: string }> = {
  tag: { icon: Tag, className: 'bg-prov-tag-soft text-prov-tag' },
  path: { icon: FolderOpen, className: 'bg-prov-path-soft text-prov-path' },
  edited: { icon: Lock, className: 'bg-prov-edit-soft text-prov-edit' },
  community: { icon: Globe, className: 'bg-prov-community-soft text-prov-community' },
};

const pill =
  'inline-flex h-5 shrink-0 items-center gap-1 rounded-[6px] pr-[7px] pl-[5px] text-[11px] font-[650] tracking-[0.01em] whitespace-nowrap [&_svg]:size-3';

export function ProvenanceMarker({
  source,
  short,
  className,
}: {
  source: FieldSource;
  short?: boolean;
  className?: string;
}) {
  const { t } = useTranslation();
  if (!source) return null;
  const { icon: Icon, className: tone } = SOURCES[source];
  const label = t(`provenance.${source}`);
  return (
    <span
      className={cn(pill, tone, short && 'px-[5px]', className)}
      title={t(`provenance.${source}Hint`)}
      aria-label={short ? label : undefined}
    >
      <Icon aria-hidden="true" strokeWidth={2.2} />
      {short ? null : label}
    </span>
  );
}

/** A field edited on the page but not saved yet. */
export function UnsavedMarker({ className }: { className?: string }) {
  const { t } = useTranslation();
  return (
    <span
      className={cn(
        pill,
        'text-brand-ink outline-1 -outline-offset-1 outline-brand outline-dashed',
        className,
      )}
    >
      {t('provenance.unsaved')}
    </span>
  );
}
