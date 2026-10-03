import { useTranslation } from 'react-i18next';
import type { Destination } from '@/components/shell/destinations';
import { Page } from '@/components/page';

/**
 * The designed placeholder for a section a later redesign phase builds: what
 * will live here, and which phase brings it.
 */
export function ComingSoon({
  destination,
  section,
}: {
  destination: Destination;
  section: string;
}) {
  const { t } = useTranslation();
  const Icon = destination.icon;
  return (
    <Page>
      <div className="flex flex-col items-center gap-2.5 rounded-xl border bg-card px-6 py-14 text-center">
        <span className="mb-2 grid size-12 place-items-center rounded-[14px] bg-brand-soft text-brand-ink">
          <Icon className="size-6" aria-hidden="true" />
        </span>
        <div className="eyebrow">{t('soon.eyebrow')}</div>
        <h2 className="h2">
          {t('soon.title', { section: t(`shell.section.${destination.key}.${section}`) })}
        </h2>
        <p className="max-w-[520px] text-muted-foreground">{t(`soon.body.${destination.key}`)}</p>
        <p className="max-w-[520px] text-[12.5px] text-subtle-foreground">
          {t('soon.phase', { phase: destination.pending[section] ?? '' })}
        </p>
      </div>
    </Page>
  );
}
