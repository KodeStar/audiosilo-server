import { useTranslation } from 'react-i18next';
import { Languages } from 'lucide-react';
import { LANGUAGES, setLanguage, type Language } from '@/i18n';

/** A compact language picker (native select: keyboard and screen-reader friendly for free). */
export function LanguageSelect() {
  const { t, i18n } = useTranslation();
  return (
    <label className="inline-flex items-center gap-1.5 text-muted-foreground">
      <Languages className="size-4" aria-hidden="true" />
      <span className="sr-only">{t('ui.language')}</span>
      <select
        className="h-8 rounded-sm border border-input bg-card px-2 text-[13px] text-foreground"
        value={i18n.resolvedLanguage}
        onChange={(e) => setLanguage(e.target.value as Language)}
      >
        {Object.entries(LANGUAGES).map(([code, label]) => (
          <option key={code} value={code}>
            {label}
          </option>
        ))}
      </select>
    </label>
  );
}
