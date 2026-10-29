import { useCallback, useId, useMemo, useState } from 'react';
import { Link } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { Eye } from 'lucide-react';
import { useBookLibraries, useDuplicates, useLibraryRoots } from '@/api/hooks';
import type { DuplicateGroup, DuplicateMember } from '@/api/types';
import { BookCover } from '@/components/book-cover';
import { LoadMore } from '@/components/load-more';
import { QueryError } from '@/components/query-error';
import { StatusText } from '@/components/status-text';
import { Badge } from '@/components/ui/badge';
import { Button, buttonVariants } from '@/components/ui/button';
import { Switch } from '@/components/ui/switch';
import { audioLine } from '@/features/book/book-model';
import { bookRoute, refKey } from '@/lib/book-route';
import { formatBytes, formatDuration, formatNumber, formatRelative } from '@/lib/format';
import { joinLibraryPath } from '@/lib/paths';
import { useInView } from '@/lib/use-in-view';
import { cn } from '@/lib/utils';
import { AllClear } from './all-clear';
import { useIssueActions, type IssueActions } from './use-issue-actions';

/**
 * Likely duplicates (STYLEGUIDE.md "Duplicates use a side-by-side compare"): each
 * group's copies side by side, the one worth keeping first. Nothing here touches
 * files; "They're different books" stops the group being suggested (until a new
 * copy turns up). With `ignored`, the groups an admin said are different. Books
 * sharing an ASIN/ISBN at different lengths may be different recordings, so no
 * copy is called the one to keep there: the card says what to check instead.
 * The groups come a page at a time (more as the list scrolls), each copy's cover
 * once its column nears the viewport, and a group of many copies shows the first
 * few until asked for the rest.
 */
export function Duplicates({
  ignored,
  across,
  onAcrossChange,
}: {
  ignored: boolean;
  /** Copies in different libraries are usually on purpose: grouped only when asked (#165). */
  across: boolean;
  onAcrossChange: (on: boolean) => void;
}) {
  const groups = useDuplicates(ignored, across);
  const actions = useIssueActions('duplicate');
  const roots = useLibraryRoots();
  const bookLibraries = useBookLibraries();
  // One tree whatever the state, so the switch keeps its focus while the list loads.
  const several = (bookLibraries?.length ?? 0) > 1;
  return (
    <div className="flex flex-col gap-4">
      {several ? <AcrossToggle on={across} onChange={onAcrossChange} /> : null}
      <DuplicateList ignored={ignored} groups={groups} actions={actions} roots={roots} />
    </div>
  );
}

function DuplicateList({
  ignored,
  groups,
  actions,
  roots,
}: {
  ignored: boolean;
  groups: ReturnType<typeof useDuplicates>;
  actions: IssueActions;
  roots: Record<number, string>;
}) {
  const { t } = useTranslation();
  if (groups.isError) {
    return (
      <QueryError
        title={t('health.listError')}
        error={groups.error}
        onRetry={() => void groups.refetch()}
      />
    );
  }
  if (!groups.data) {
    return (
      <div className="skel h-[260px] rounded-xl" role="status" aria-label={t('common.loading')} />
    );
  }
  return <DuplicatePages ignored={ignored} groups={groups} actions={actions} roots={roots} />;
}

function DuplicatePages({
  ignored,
  groups,
  actions,
  roots,
}: {
  ignored: boolean;
  groups: ReturnType<typeof useDuplicates>;
  actions: IssueActions;
  roots: Record<number, string>;
}) {
  const pages = groups.data?.pages;
  // The server lists the open, or only the ignored, groups. A group can shift
  // across a page boundary between two page loads (a copy came or went): shown once.
  const shown = useMemo(() => {
    const seen = new Set<string>();
    return (pages ?? [])
      .flatMap((p) => p.groups)
      .filter((g) => {
        const key = refKey(g.books[0]);
        if (seen.has(key)) return false;
        seen.add(key);
        return true;
      });
  }, [pages]);
  const total = Math.max(pages?.[0]?.total ?? 0, shown.length);
  const { fetchNextPage } = groups;
  const more = useCallback(() => void fetchNextPage(), [fetchNextPage]);
  if (shown.length === 0) return <AllClear ignored={ignored} />;
  return (
    <>
      {shown.map((g, i) => (
        <GroupCard
          key={refKey(g.books[0])}
          group={g}
          index={i}
          total={total}
          actions={actions}
          roots={roots}
        />
      ))}
      {groups.hasNextPage ? (
        <LoadMore
          onMore={more}
          loading={groups.isFetchingNextPage}
          loaded={pages?.length ?? 0}
          auto={!groups.isFetchNextPageError}
        />
      ) : null}
    </>
  );
}

/** "Include other libraries": also group copies in different libraries. */
function AcrossToggle({ on, onChange }: { on: boolean; onChange: (on: boolean) => void }) {
  const { t } = useTranslation();
  const id = useId();
  return (
    <div className="flex items-start gap-3 rounded-xl border bg-card px-4 py-3">
      <Switch
        id={id}
        checked={on}
        onCheckedChange={onChange}
        aria-describedby={`${id}-hint`}
        className="mt-0.5"
      />
      <div className="flex min-w-0 flex-col gap-0.5">
        <label htmlFor={id} className="font-medium">
          {t('health.dup.across')}
        </label>
        <span id={`${id}-hint`} className="text-[12.5px] text-muted-foreground">
          {t('health.dup.acrossHint')}
        </span>
      </div>
    </div>
  );
}

/** The copies a group shows until asked for the rest. */
const COPIES_SHOWN = 10;

function GroupCard({
  group: g,
  index,
  total,
  actions,
  roots,
}: {
  group: DuplicateGroup;
  index: number;
  total: number;
  actions: IssueActions;
  roots: Record<number, string>;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const keep = g.books[0];
  // Copies in more than one library (?across=1): each names its library.
  const spans = new Set(g.books.map((b) => b.library_id)).size > 1;
  // Maybe different recordings: no copy is suggested over another.
  const unsure = g.reason === 'id_length_differs';
  // A group of many copies (identical audio across a big library) shows a few.
  const [all, setAll] = useState(false);
  const copies = all ? g.books : g.books.slice(0, COPIES_SHOWN);
  return (
    <section className="rounded-xl border bg-card" aria-labelledby={`dup-${index}`}>
      <div className="flex flex-wrap items-start justify-between gap-3 border-b px-5 py-4">
        <div className="flex min-w-0 flex-col gap-0.5">
          <h3 id={`dup-${index}`} className="h3 [overflow-wrap:anywhere]">
            {keep.title || keep.path}
          </h3>
          <span className="text-[12.5px] text-muted-foreground">
            {t(`health.dup.reason.${g.reason}`, { count: g.books.length })}
          </span>
          {/* What to check: moot once the admin has said they're different books. */}
          {unsure && !g.ignored && (
            <StatusText tone="warn" colored className="text-[12.5px]">
              {t('health.dup.lengthDiffers')}
            </StatusText>
          )}
        </div>
        <Badge variant="info">
          {t('health.dup.position', {
            n: formatNumber(index + 1, lang),
            total: formatNumber(total, lang),
          })}
        </Badge>
      </div>
      <div className="grid grid-cols-1 md:grid-cols-2">
        {copies.map((b, i) => (
          <CopyColumn
            key={refKey(b)}
            book={b}
            keep={i === 0}
            unsure={unsure}
            library={spans ? b.library_name : undefined}
            root={roots[b.library_id]}
            lang={lang}
          />
        ))}
      </div>
      <div className="flex flex-wrap items-center justify-end gap-2 border-t px-5 py-3">
        {copies.length < g.books.length ? (
          <Button variant="outline" size="sm" className="mr-auto" onClick={() => setAll(true)}>
            {t('health.dup.showAll', { n: formatNumber(g.books.length, lang) })}
          </Button>
        ) : null}
        {g.ignored ? (
          <Button variant="outline" size="sm" onClick={() => void actions.unignore(g.books)}>
            <Eye aria-hidden="true" />
            {t('health.dup.suggestAgain')}
          </Button>
        ) : (
          <Button variant="ghost" size="sm" onClick={() => void actions.ignore(g.books)}>
            {t('health.dup.different')}
          </Button>
        )}
      </div>
    </section>
  );
}

function CopyColumn({
  book: b,
  keep,
  unsure,
  library,
  root,
  lang,
}: {
  book: DuplicateMember;
  keep: boolean;
  /** Maybe a different recording: neither "worth keeping" nor "another copy". */
  unsure: boolean;
  /** The copy's library, named when the group's copies are in more than one. */
  library: string | undefined;
  root: string | undefined;
  lang: string;
}) {
  const { t } = useTranslation();
  // The cover is asked for once the column nears the viewport.
  const [ref, seen] = useInView<HTMLDivElement>();
  const none = t('health.dup.none');
  const rows: [string, React.ReactNode, boolean?][] = [
    ...(library !== undefined
      ? ([[t('health.dup.library'), library]] as [string, React.ReactNode][])
      : []),
    [t('health.dup.path'), joinLibraryPath(root, b.path), true],
    [t('health.dup.format'), audioLine(b, t)],
    [
      t('health.dup.length'),
      b.duration ? formatDuration(b.duration, lang) : t('health.dup.unknown'),
    ],
    [t('health.dup.size'), formatBytes(b.size, lang)],
    [t('health.dup.chapters'), b.chapter_count > 1 ? formatNumber(b.chapter_count, lang) : none],
    [t('health.dup.matched'), b.matched ? t('health.dup.yes') : t('health.dup.no')],
    [t('health.dup.listeners'), b.listeners ? formatNumber(b.listeners, lang) : none],
    [t('health.dup.added'), formatRelative(b.added_at, lang)],
  ];
  return (
    <div
      ref={ref}
      className={cn(
        'flex min-w-0 flex-col gap-3 p-5',
        !keep && 'border-t md:border-t-0 md:border-l',
      )}
    >
      <div className="flex items-center gap-3">
        <Link
          {...bookRoute(b.library_id, b.path)}
          className="w-16 shrink-0"
          aria-label={t('health.open', { title: b.title || b.path })}
        >
          <BookCover
            libraryId={b.library_id}
            path={b.path}
            title={b.title}
            author={b.author}
            size={160}
            load={seen}
            version={b.cover_version}
          />
        </Link>
        <div className="flex flex-col items-start gap-1.5">
          {!unsure && (
            <Badge variant={keep ? 'success' : 'outline'}>
              {keep ? t('health.dup.keep') : t('health.dup.copy')}
            </Badge>
          )}
          <Link
            {...bookRoute(b.library_id, b.path)}
            className={buttonVariants({ variant: 'link', size: 'sm' })}
          >
            {t('health.dup.openBook')}
          </Link>
        </div>
      </div>
      <dl className="grid grid-cols-[minmax(84px,auto)_1fr] gap-x-4 gap-y-1.5 text-[13px]">
        {rows.map(([label, value, mono]) => (
          <div key={label} className="contents">
            <dt className="text-muted-foreground">{label}</dt>
            <dd className={cn('min-w-0 [overflow-wrap:anywhere]', mono && 'font-mono text-[12px]')}>
              {value}
            </dd>
          </div>
        ))}
      </dl>
    </div>
  );
}
