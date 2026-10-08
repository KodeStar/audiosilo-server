import { useId } from 'react';
import { useTranslation } from 'react-i18next';
import { TriangleAlert } from 'lucide-react';

/**
 * The style guide's Danger zone: a separate bordered block at the bottom of a
 * page, red header, one `SettingRow` per irreversible action.
 */
export function DangerZone({ children }: { children: React.ReactNode }) {
  const { t } = useTranslation();
  const id = useId();
  return (
    <section
      aria-labelledby={id}
      className="overflow-hidden rounded-xl border border-[color-mix(in_oklab,var(--destructive)_35%,var(--border))] bg-card"
    >
      <h2
        id={id}
        className="flex items-center gap-2 border-b border-[color-mix(in_oklab,var(--destructive)_25%,var(--border))] bg-destructive-soft px-5 py-3 text-[14px] font-[650] text-destructive"
      >
        <TriangleAlert className="size-4" aria-hidden="true" />
        {t('dangerZone.title')}
      </h2>
      <div className="divide-y px-5">{children}</div>
    </section>
  );
}
