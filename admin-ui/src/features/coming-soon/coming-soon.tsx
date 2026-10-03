import { useParams } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { History } from 'lucide-react';
import type { Destination } from '@/components/shell/destinations';
import { buttonVariants } from '@/components/ui/button';
import { NotFound } from '@/features/not-found';

/**
 * The designed placeholder for a destination whose screens a later phase
 * builds. It says what will live here and points at the classic console, which
 * keeps working until the cutover (Phase 1b).
 */
export function ComingSoon({ destination }: { destination: Destination }) {
  const { t } = useTranslation();
  const { section } = useParams({ strict: false }) as { section?: string };
  const active = section ?? destination.sections[0];
  if (!destination.sections.includes(active)) return <NotFound />;
  const Icon = destination.icon;

  return (
    <div className="mx-auto max-w-[1440px] px-4 pt-5 pb-[120px] min-[721px]:px-6 min-[721px]:pt-7">
      <div className="flex flex-col items-center gap-2.5 rounded-xl border bg-card px-6 py-14 text-center">
        <span className="mb-2 grid size-12 place-items-center rounded-[14px] bg-brand-soft text-brand-ink">
          <Icon className="size-6" aria-hidden="true" />
        </span>
        <div className="eyebrow">{t('soon.eyebrow')}</div>
        <h2 className="h2">
          {t('soon.title', { section: t(`shell.section.${destination.key}.${active}`) })}
        </h2>
        <p className="max-w-[520px] text-muted-foreground">{t(`soon.body.${destination.key}`)}</p>
        <p className="max-w-[520px] text-[12.5px] text-subtle-foreground">
          {t('soon.phase', { phase: destination.phase })}
        </p>
        <a
          href="/admin/classic"
          className={buttonVariants({ variant: 'outline', className: 'mt-3' })}
        >
          <History aria-hidden="true" />
          {t('shell.account.classic')}
        </a>
      </div>
    </div>
  );
}
