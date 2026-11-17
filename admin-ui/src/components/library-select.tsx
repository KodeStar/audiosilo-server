import { useTranslation } from 'react-i18next';
import { useBookLibraries, useLibraries } from '@/api/hooks';
import { NativeSelect } from '@/components/ui/native-select';

/**
 * Which library an action works on, or all of them (0): Match automatically,
 * Clear community matches. Book libraries only (a podcast library has no books
 * to match), unless `every` (Jobs' scan history: podcast libraries are scanned
 * too); hidden with a single one, where there is no choice to make.
 */
export function LibrarySelect({
  value,
  onChange,
  label,
  every = false,
}: {
  value: number;
  onChange: (id: number) => void;
  label: string;
  every?: boolean;
}) {
  const { t } = useTranslation();
  const all = useLibraries().data;
  const books = useBookLibraries();
  const libraries = (every ? all : books) ?? [];
  if (libraries.length < 2) return null;
  return (
    <NativeSelect
      aria-label={label}
      className="w-auto max-w-full"
      value={String(value)}
      onChange={(e) => onChange(Number(e.target.value))}
    >
      <option value="0">{t('common.allLibraries')}</option>
      {libraries.map((l) => (
        <option key={l.id} value={l.id}>
          {l.name}
        </option>
      ))}
    </NativeSelect>
  );
}
