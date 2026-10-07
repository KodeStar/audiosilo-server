import { useTranslation } from 'react-i18next';
import { BookOpen, Mic, PenLine, Search, Share2 } from 'lucide-react';
import {
  PALETTE_STALE_MS,
  useAdminBookPage,
  usePeople,
  useSeries,
  useShares,
  useUsers,
} from '@/api/hooks';
import { BookCover } from '@/components/book-cover';
import { Monogram } from '@/components/monogram';
import { bookRoute, refKey } from '@/lib/book-route';
import { seriesLabel } from '@/lib/format';
import { useDebounced } from '@/lib/use-debounced';
import { topMatches } from './palette-filter';
import type { Go, PaletteEntry } from './command-palette';

/** Searches shorter than this only filter the static entries (no requests). */
export const MIN_SEARCH = 2;
const PER_GROUP = 4;

/**
 * The palette's content search (STYLEGUIDE.md "Command palette"): books from the
 * server's full-text search, then people, authors, series, narrators and shares
 * matched here with the palette's own rule. Nothing loads until the search has
 * MIN_SEARCH characters, the book search waits for typing to pause, and the
 * aggregates stay fresh for a few minutes (the palette reopens often).
 */
export function usePaletteSearch(
  search: string,
  go: Go,
): {
  groups: { heading: string; entries: PaletteEntry[] }[];
  /** "Search all books for …": offered for any search, after every real match. */
  fallback: PaletteEntry[];
  searchingBooks: boolean;
} {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const q = search.trim();
  const active = q.length >= MIN_SEARCH;
  const settled = useDebounced(q, 200);
  const booksQuery = useAdminBookPage(
    { q: settled, limit: 6 },
    active && settled.length >= MIN_SEARCH,
  );
  const authors = usePeople('author', undefined, active, PALETTE_STALE_MS);
  const narrators = usePeople('narrator', undefined, active, PALETTE_STALE_MS);
  const series = useSeries(undefined, active, PALETTE_STALE_MS);
  const users = useUsers(active, PALETTE_STALE_MS);
  const shares = useShares(active, PALETTE_STALE_MS);
  if (!active) return { groups: [], fallback: [], searchingBooks: false };

  const toBooks = (filter: Record<string, string>) =>
    go('/library/{-$section}', { section: undefined }, filter);

  // The server matched these on fields the palette can't see: shown as they come.
  const books = (settled === q ? (booksQuery.data?.books ?? []) : []).map<PaletteEntry>((b) => ({
    id: `book:${refKey(b)}`,
    title: b.title,
    subtitle: [b.author, seriesLabel(b.series, b.series_index, lang, t), b.library_name]
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
    run: () => {
      const r = bookRoute(b.library_id, b.path);
      go(r.to, undefined, r.search);
    },
  }));

  const people = topMatches(users.data ?? [], (u) => u.username, q, PER_GROUP).map<PaletteEntry>(
    (u) => ({
      id: `user-${u.id}`,
      title: u.username,
      subtitle: t(`people.role.${u.role}`),
      visual: <Monogram name={u.username} size={36} />,
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
      run: () => toBooks({ [kind]: x.name }),
    }));

  const shareEntries = topMatches(shares.data ?? [], (s) => s.name, q, PER_GROUP).map<PaletteEntry>(
    (s) => ({
      id: `share-${s.id}`,
      title: s.name,
      subtitle: t('palette.shareSub', { count: s.member_ids?.length ?? 0 }),
      icon: Share2,
      run: () => go('/people/{-$section}', { section: 'shares' }, { share: s.id }),
    }),
  );

  return {
    groups: [
      { heading: t('palette.group.books'), entries: books },
      { heading: t('palette.group.people'), entries: people },
      {
        heading: t('palette.group.authors'),
        entries: named(authors.data?.people, 'author', PenLine),
      },
      { heading: t('palette.group.series'), entries: named(series.data, 'series', BookOpen) },
      {
        heading: t('palette.group.narrators'),
        entries: named(narrators.data?.people, 'narrator', Mic),
      },
      { heading: t('palette.group.shares'), entries: shareEntries },
    ],
    fallback: [
      {
        id: 'search-books',
        title: t('palette.searchBooks', { query: q }),
        subtitle: `${t('shell.dest.library')} › ${t('shell.section.library.books')}`,
        icon: Search,
        run: () => toBooks({ q }),
      },
    ],
    searchingBooks: settled !== q || booksQuery.isFetching,
  };
}
