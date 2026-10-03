import { useEffect, useRef } from 'react';
import { Link, useMatch, useParams, useRouterState } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { ArrowLeft, ChevronRight } from 'lucide-react';
import { useAdminBook, useLibraries, useUser } from '@/api/hooks';
import { buttonVariants } from '@/components/ui/button';
import { segmentClass, segmentTrack } from '@/components/ui/segment-classes';
import { cn } from '@/lib/utils';
import { destinationFor } from './destinations';

/**
 * The 48px row under the top bar: the destination's title and its sections as a
 * segmented control. Home has no sub bar; a detail page (a person, a book) shows
 * a back button and a breadcrumb instead of the sections.
 */
export function SubBar() {
  const { t } = useTranslation();
  const pathname = useRouterState({ select: (s) => s.location.pathname });
  const params = useParams({ strict: false }) as { section?: string };
  const person = useMatch({ from: '/people/user/$userId', shouldThrow: false });
  const book = useMatch({ from: '/library/book', shouldThrow: false });
  const activeRef = useRef<HTMLAnchorElement>(null);
  const dest = destinationFor(pathname);
  // On a phone the sections scroll sideways; keep the current one on screen.
  useEffect(() => {
    activeRef.current?.scrollIntoView({ block: 'nearest', inline: 'nearest' });
  }, [pathname]);
  if (!dest) return null;
  if (person) return <PersonCrumbs userId={Number(person.params.userId)} />;
  if (book) return <BookCrumbs libraryId={book.search.library} path={book.search.path} />;
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

/** Library › <library> › <author> › <title>; the library and author give way on phones. */
function BookCrumbs({ libraryId, path }: { libraryId: number; path: string }) {
  const { t } = useTranslation();
  return (
    <div className="border-t border-topbar-border">
      <div className="mx-auto flex h-[46px] max-w-[1440px] items-center gap-2 px-4 md:h-12 md:px-6">
        <Link
          to="/library/{-$section}"
          params={{ section: undefined }}
          className={buttonVariants({ variant: 'ghost', size: 'icon-sm' })}
          aria-label={t('shell.backTo', { page: t('shell.dest.library') })}
        >
          <ArrowLeft className="size-4" aria-hidden="true" />
        </Link>
        <nav aria-label={t('shell.breadcrumb')} className="min-w-0">
          <ol className="flex min-w-0 items-center gap-1.5 text-[13.5px]">
            <li className="shrink-0">
              <Link
                to="/library/{-$section}"
                params={{ section: undefined }}
                className="font-[550] text-muted-foreground hover:text-foreground"
              >
                {t('shell.dest.library')}
              </Link>
            </li>
            {libraryId > 0 && path ? <BookTrail libraryId={libraryId} path={path} /> : null}
          </ol>
        </nav>
      </div>
    </div>
  );
}

function BookTrail({ libraryId, path }: { libraryId: number; path: string }) {
  const book = useAdminBook(libraryId, path).data?.book;
  const library = useLibraries().data?.find((l) => l.id === libraryId);
  const libraryName = library?.name ?? book?.library_name;
  const sep = (phone: boolean) => (
    <li aria-hidden="true" className={cn('shrink-0', !phone && 'hidden md:block')}>
      <ChevronRight className="size-3.5 text-subtle-foreground" />
    </li>
  );
  const crumb =
    'block max-w-[22vw] truncate font-[550] text-muted-foreground hover:text-foreground';
  return (
    <>
      {libraryName ? (
        <>
          {sep(false)}
          <li className="hidden min-w-0 md:block">
            <Link
              to="/library/{-$section}"
              params={{ section: undefined }}
              search={{ library: libraryId }}
              className={crumb}
            >
              {libraryName}
            </Link>
          </li>
        </>
      ) : null}
      {book?.author ? (
        <>
          {sep(false)}
          <li className="hidden min-w-0 md:block">
            <Link
              to="/library/{-$section}"
              params={{ section: undefined }}
              search={{ author: book.author }}
              className={crumb}
            >
              {book.author}
            </Link>
          </li>
        </>
      ) : null}
      {sep(true)}
      <li aria-current="page" className="min-w-0 truncate font-semibold">
        {book?.title ?? '…'}
      </li>
    </>
  );
}
