import { useTranslation } from 'react-i18next';
import { BookOpen, Mic, PenLine, Search, Share2 } from 'lucide-react';
import {
  useAdminBookPage,
  useAuthors,
  useNarrators,
  useSeries,
  useShares,
  useUsers,
} from '@/api/hooks';
import { BookCover } from '@/components/book-cover';
import { Monogram } from '@/components/monogram';
import { bookRoute } from '@/lib/book-route';
import { useDebounced } from '@/lib/use-debounced';
import { FALLBACK_PREFIX, topMatches } from './palette-filter';
import type { PaletteEntry } from './command-palette';

/** Searches shorter than this only filter the static entries (no requests). */
export const MIN_SEARCH = 2;
const PER_GROUP = 4;

type Go = (
  to: string,
  params?: Record<string, string | undefined>,
  search?: Record<string, unknown>,
) => void;

/**
 * The palette's content search (STYLEGUIDE.md "Command palette"): books from the
 * server's full-text search, then people, authors, series, narrators and shares
 * matched here. Nothing loads until the search has MIN_SEARCH characters, and the
 * book search waits for typing to pause. Entries are already filtered, so they
 * are force-mounted: cmdk's own filter must not drop a full-text hit whose words
 * matched a field it can't see.
 */
export function usePaletteSearch(
  search: string,
  go: Go,
): {
  groups: { heading: string; entries: PaletteEntry[] }[];
  /** "Search all books for …": a group of its own, last, after every real match. */
  fallback: PaletteEntry[];
  /** How many content results there are (the fallback not counted). */
  count: number;
  searchingBooks: boolean;
} {
  const { t } = useTranslation();
  const q = search.trim();
  const active = q.length >= MIN_SEARCH;
  const settled = useDebounced(q, 200);
  const booksQuery = useAdminBookPage(
    { q: settled, limit: 6 },
    active && settled.length >= MIN_SEARCH,
  );
  const authors = useAuthors(undefined, active);
  const narrators = useNarrators(undefined, active);
  const series = useSeries(undefined, active);
  const users = useUsers(active);
  const shares = useShares(active);
  if (!active) return { groups: [], fallback: [], count: 0, searchingBooks: false };

  const toBooks = (filter: Record<string, string>) =>
    go('/library/{-$section}', { section: undefined }, filter);

  const books: PaletteEntry[] = [
    ...(settled === q ? (booksQuery.data?.books ?? []) : []).map<PaletteEntry>((b) => ({
      id: `book-${b.library_id}-${b.path}`,
      title: b.title,
      subtitle: [
        b.author,
        b.series && (b.series_index > 0 ? `${b.series} #${b.series_index}` : b.series),
        b.library_name,
      ]
        .filter(Boolean)
        .join(' · '),
      visual: (
        <BookCover
          libraryId={b.library_id}
          path={b.path}
          title={b.title}
          author={b.author}
          size={160}
          className="w-9 shrink-0"
        />
      ),
      keywords: [b.author, b.series, b.narrator],
      forceMount: true,
      run: () => {
        const r = bookRoute(b.library_id, b.path);
        go(r.to, undefined, r.search);
      },
    })),
  ];

  const people = topMatches(users.data ?? [], (u) => u.username, q, PER_GROUP).map<PaletteEntry>(
    (u) => ({
      id: `user-${u.id}`,
      title: u.username,
      subtitle: t(`people.role.${u.role}`),
      visual: <Monogram name={u.username} size={36} />,
      forceMount: true,
      run: () => go('/people/user/$userId', { userId: String(u.id) }),
    }),
  );

  const named = <T extends { name: string; books: number }>(
    list: readonly T[] | undefined,
    kind: 'author' | 'series' | 'narrator',
    icon: PaletteEntry['icon'],
  ) =>
    topMatches(list ?? [], (x) => x.name, q, PER_GROUP).map<PaletteEntry>((x) => ({
      id: `${kind}-${x.name}`,
      title: x.name,
      subtitle: t(`palette.${kind}Sub`, { count: x.books }),
      icon,
      forceMount: true,
      run: () => toBooks({ [kind]: x.name }),
    }));

  const shareEntries = topMatches(shares.data ?? [], (s) => s.name, q, PER_GROUP).map<PaletteEntry>(
    (s) => ({
      id: `share-${s.id}`,
      title: s.name,
      subtitle: t('palette.shareSub', { count: s.member_ids?.length ?? 0 }),
      icon: Share2,
      forceMount: true,
      run: () => go('/people/{-$section}', { section: 'shares' }, { share: s.id }),
    }),
  );

  const groups = [
    { heading: t('palette.group.books'), entries: books },
    { heading: t('palette.group.people'), entries: people },
    {
      heading: t('palette.group.authors'),
      entries: named(authors.data?.authors, 'author', PenLine),
    },
    { heading: t('palette.group.series'), entries: named(series.data, 'series', BookOpen) },
    {
      heading: t('palette.group.narrators'),
      entries: named(narrators.data?.narrators, 'narrator', Mic),
    },
    { heading: t('palette.group.shares'), entries: shareEntries },
  ];
  return {
    groups,
    count: groups.reduce((n, g) => n + g.entries.length, 0),
    fallback: [
      {
        id: `${FALLBACK_PREFIX}books`,
        title: t('palette.searchBooks', { query: q }),
        subtitle: `${t('shell.dest.library')} › ${t('shell.section.library.books')}`,
        icon: Search,
        forceMount: true,
        run: () => toBooks({ q }),
      },
    ],
    searchingBooks: settled !== q || booksQuery.isFetching,
  };
}
