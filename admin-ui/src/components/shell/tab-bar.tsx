import { Link, useRouterState } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { DESTINATIONS, destinationFor } from './destinations';

/** Mobile (<720px) primary navigation: the five destinations as a bottom tab bar. */
export function TabBar() {
  const { t } = useTranslation();
  const pathname = useRouterState({ select: (s) => s.location.pathname });
  const current = destinationFor(pathname);
  return (
    <nav
      className="fixed inset-x-0 bottom-0 z-45 flex border-t border-topbar-border bg-topbar px-1.5 pt-1.5 pb-[calc(6px+env(safe-area-inset-bottom))] backdrop-blur-[16px] backdrop-saturate-[1.6] min-[721px]:hidden"
      aria-label={t('shell.primaryNav')}
    >
      {DESTINATIONS.map((d) => {
        const active = current?.key === d.key;
        return (
          <Link
            key={d.key}
            to={d.route}
            params={{ section: undefined }}
            aria-current={active ? 'page' : undefined}
            className="flex min-h-11 min-w-0 flex-1 flex-col items-center justify-center gap-[3px] rounded-md py-1 text-[10.5px] font-semibold text-muted-foreground aria-[current=page]:text-foreground aria-[current=page]:[&>svg]:text-brand"
          >
            <d.icon className="size-[22px]" aria-hidden="true" />
            <span className="max-w-full truncate">{t(`shell.dest.${d.key}`)}</span>
          </Link>
        );
      })}
    </nav>
  );
}
