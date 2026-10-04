import { Link } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { Eye } from 'lucide-react';
import { useDuplicates, useLibraries } from '@/api/hooks';
import type { DuplicateGroup, DuplicateMember } from '@/api/types';
import { BookCover } from '@/components/book-cover';
import { QueryError } from '@/components/query-error';
import { Badge } from '@/components/ui/badge';
import { Button, buttonVariants } from '@/components/ui/button';
import { bookRoute, refKey } from '@/lib/book-route';
import { formatBytes, formatDuration, formatNumber, formatRelative } from '@/lib/format';
import { joinLibraryPath } from '@/lib/paths';
import { cn } from '@/lib/utils';
import { AllClear } from './all-clear';
import { useIssueActions } from './use-issue-actions';

/**
 * Likely duplicates (STYLEGUIDE.md "Duplicates use a side-by-side compare"): each
 * group's copies side by side, the one worth keeping first. Nothing here touches
 * files; "They're different books" stops the group being suggested (until a new
 * copy turns up). With `ignored`, the groups an admin said are different.
 */
export function Duplicates({ ignored }: { ignored: boolean }) {
  const { t } = useTranslation();
  const groups = useDuplicates(true, ignored);
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
  const shown = ignored ? groups.data.filter((g) => g.ignored) : groups.data;
  if (shown.length === 0) {
    return <AllClear ignored={ignored} />;
  }
  return (
    <div className="flex flex-col gap-4">
      {shown.map((g, i) => (
        <GroupCard key={refKey(g.books[0])} group={g} index={i} total={shown.length} />
      ))}
    </div>
  );
}

function GroupCard({
  group: g,
  index,
  total,
}: {
  group: DuplicateGroup;
  index: number;
  total: number;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const actions = useIssueActions('duplicate');
  const keep = g.books[0];
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
        </div>
        <Badge variant="info">
          {t('health.dup.position', {
            n: formatNumber(index + 1, lang),
            total: formatNumber(total, lang),
          })}
        </Badge>
      </div>
      <div className="grid md:grid-cols-2">
        {g.books.map((b, i) => (
          <CopyColumn key={refKey(b)} book={b} keep={i === 0} lang={lang} />
        ))}
      </div>
      <div className="flex flex-wrap items-center justify-end gap-2 border-t px-5 py-3">
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
  lang,
}: {
  book: DuplicateMember;
  keep: boolean;
  lang: string;
}) {
  const { t } = useTranslation();
  const root = useLibraries().data?.find((l) => l.id === b.library_id)?.root;
  const rows: [string, React.ReactNode, boolean?][] = [
    [t('health.dup.path'), joinLibraryPath(root, b.path), true],
    [
      t('health.dup.format'),
      [
        b.format.toUpperCase(),
        b.codec,
        b.file_count > 1 ? t('health.dup.files', { count: b.file_count }) : '',
      ]
        .filter(Boolean)
        .join(' · '),
    ],
    [
      t('health.dup.length'),
      b.duration ? formatDuration(b.duration, lang) : t('health.dup.unknown'),
    ],
    [t('health.dup.size'), formatBytes(b.size, lang)],
    [
      t('health.dup.chapters'),
      b.chapter_count > 1 ? formatNumber(b.chapter_count, lang) : t('health.dup.none'),
    ],
    [t('health.dup.matched'), b.matched ? t('health.dup.yes') : t('health.dup.no')],
    [
      t('health.dup.listeners'),
      b.listeners ? formatNumber(b.listeners, lang) : t('health.dup.none'),
    ],
    [t('health.dup.added'), formatRelative(b.added_at, lang)],
  ];
  return (
    <div
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
          />
        </Link>
        <div className="flex flex-col items-start gap-1.5">
          <Badge variant={keep ? 'success' : 'outline'}>
            {keep ? t('health.dup.keep') : t('health.dup.copy')}
          </Badge>
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
