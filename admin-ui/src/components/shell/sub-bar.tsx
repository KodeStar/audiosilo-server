import { Link, useParams, useRouterState } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { cn } from '@/lib/utils';
import { destinationFor } from './destinations';

/**
 * The 48px row under the top bar: the destination's title and its sections as a
 * segmented control. Home has no sub bar.
 */
export function SubBar() {
  const { t } = useTranslation();
  const pathname = useRouterState({ select: (s) => s.location.pathname });
  const params = useParams({ strict: false }) as { section?: string };
  const dest = destinationFor(pathname);
  if (!dest) return null;
  const active = params.section ?? dest.sections[0];

  return (
    <div className="border-t border-topbar-border">
      <div className="mx-auto flex h-[46px] max-w-[1440px] items-center gap-4 px-4 min-[721px]:h-12 min-[721px]:px-6">
        <h1 className="hidden text-[15px] font-bold tracking-[-0.01em] whitespace-nowrap min-[721px]:block">
          {t(`shell.dest.${dest.key}`)}
        </h1>
        <nav
          className="hscroll inline-flex max-w-full gap-0.5 self-center rounded-[11px] border bg-muted p-[3px]"
          aria-label={t('shell.sectionsNav', { destination: t(`shell.dest.${dest.key}`) })}
        >
          {dest.sections.map((s, i) => {
            const current = s === active;
            return (
              <Link
                key={s}
                to={dest.route}
                params={{ section: i === 0 ? undefined : s }}
                aria-current={current ? 'page' : undefined}
                className={cn(
                  'inline-flex h-[30px] items-center rounded-sm px-3 text-[13px] font-[550] whitespace-nowrap text-muted-foreground transition-colors duration-(--dur-1) hover:text-foreground',
                  current &&
                    'bg-card text-foreground shadow-[0_1px_2px_rgb(18_28_54/0.08),0_0_0_1px_var(--border)]',
                )}
              >
                {t(`shell.section.${dest.key}.${s}`)}
              </Link>
            );
          })}
        </nav>
      </div>
    </div>
  );
}
