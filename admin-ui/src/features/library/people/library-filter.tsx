import { useTranslation } from 'react-i18next';
import { useLibraries } from '@/api/hooks';
import { SegmentedControl } from '@/components/ui/segmented-control';
import { useLibraryParam } from './library-param';

/**
 * All + each library, as a segmented control (Authors, Narrators, Series). Hidden
 * with a single library, where it would filter nothing.
 */
export function LibraryFilter() {
  const { t } = useTranslation();
  const libraries = useLibraries();
  const [library, setLibrary] = useLibraryParam();
  const list = libraries.data ?? [];
  if (list.length < 2) return null;
  return (
    <div className="hscroll max-w-full">
      <SegmentedControl
        label={t('library-filter.label')}
        value={library ?? 0}
        onChange={(id) => setLibrary(id || undefined)}
        options={[
          { value: 0, label: t('library-filter.all') },
          ...list.map((l) => ({ value: l.id, label: l.name })),
        ]}
      />
    </div>
  );
}
