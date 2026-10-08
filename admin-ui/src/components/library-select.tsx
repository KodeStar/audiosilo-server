import { useTranslation } from 'react-i18next';
import { useLibraries } from '@/api/hooks';
import { NativeSelect } from '@/components/ui/native-select';

/**
 * Which library an action works on, or all of them (0): Match automatically,
 * Clear community matches. Hidden with a single library, where there is no
 * choice to make.
 */
export function LibrarySelect({
  value,
  onChange,
  label,
}: {
  value: number;
  onChange: (id: number) => void;
  label: string;
}) {
  const { t } = useTranslation();
  const libraries = useLibraries().data ?? [];
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
