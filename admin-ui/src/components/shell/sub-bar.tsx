import { useEffect, useRef } from 'react';
import { Link, useMatch, useParams, useRouterState } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { ArrowLeft, ChevronRight } from 'lucide-react';
import { useUser } from '@/api/hooks';
import { buttonVariants } from '@/components/ui/button';
import { segmentClass, segmentTrack } from '@/components/ui/segment-classes';
import { cn } from '@/lib/utils';
import { destinationFor } from './destinations';

/**
 * The 48px row under the top bar: the destination's title and its sections as a
 * segmented control. Home has no sub bar; a detail page (a person) shows a back
 * button and a breadcrumb instead of the sections.
 */
export function SubBar() {
  const { t } = useTranslation();
  const pathname = useRouterState({ select: (s) => s.location.pathname });
  const params = useParams({ strict: false }) as { section?: string };
  const person = useMatch({ from: '/people/user/$userId', shouldThrow: false });
  const activeRef = useRef<HTMLAnchorElement>(null);
  const dest = destinationFor(pathname);
  // On a phone the sections scroll sideways; keep the current one on screen.
  useEffect(() => {
    activeRef.current?.scrollIntoView({ block: 'nearest', inline: 'nearest' });
  }, [pathname]);
  if (!dest) return null;
  if (person) return <PersonCrumbs userId={Number(person.params.userId)} />;
  const active = params.section ?? dest.sections[0];

  return (
    <div className="border-t border-topbar-border">
      <div className="mx-auto flex h-[46px] max-w-[1440px] items-center gap-4 px-4 md:h-12 md:px-6">
        <h1 className="hidden text-[15px] font-bold tracking-[-0.01em] whitespace-nowrap md:block">
          {t(`shell.dest.${dest.key}`)}
        </h1>
        <nav
          className={cn(segmentTrack, 'hscroll max-w-full self-center')}
          aria-label={t('shell.sectionsNav', { destination: t(`shell.dest.${dest.key}`) })}
        >
          {dest.sections.map((s, i) => {
            const current = s === active;
            return (
              <Link
                key={s}
                to={dest.route}
                params={{ section: i === 0 ? undefined : s }}
                ref={current ? activeRef : undefined}
                aria-current={current ? 'page' : undefined}
                className={segmentClass(current)}
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

function PersonCrumbs({ userId }: { userId: number }) {
  const { t } = useTranslation();
  const user = useUser(userId);
  return (
    <div className="border-t border-topbar-border">
      <div className="mx-auto flex h-[46px] max-w-[1440px] items-center gap-2 px-4 md:h-12 md:px-6">
        <Link
          to="/people/{-$section}"
          params={{ section: undefined }}
          className={buttonVariants({ variant: 'ghost', size: 'icon-sm' })}
          aria-label={t('shell.backTo', { page: t('shell.dest.people') })}
        >
          <ArrowLeft className="size-4" aria-hidden="true" />
        </Link>
        <nav aria-label={t('shell.breadcrumb')}>
          <ol className="flex items-center gap-1.5 text-[13.5px]">
            <li>
              <Link
                to="/people/{-$section}"
                params={{ section: undefined }}
                className="font-[550] text-muted-foreground hover:text-foreground"
              >
                {t('shell.dest.people')}
              </Link>
            </li>
            <li aria-hidden="true">
              <ChevronRight className="size-3.5 text-subtle-foreground" />
            </li>
            <li aria-current="page" className="max-w-[50vw] truncate font-semibold">
              {user.data?.user.username ?? '…'}
            </li>
          </ol>
        </nav>
      </div>
    </div>
  );
}
