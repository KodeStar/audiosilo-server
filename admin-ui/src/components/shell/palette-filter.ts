import { fold } from '@/lib/utils';

// The palette's one matching rule, for its own entries and the content it
// matches in the browser (people, authors, series, narrators, shares): every
// search word must appear in the title or keywords, ignoring case and accents,
// as a substring. Fuzzy scoring (cmdk's default) matches letters spread across a
// whole subtitle, so "narrators" would match "Rescan Fiction" and Enter would
// run the wrong command.

/** The search as folded words ([] for a blank search). */
const wordsOf = (search: string) => fold(search.trim()).split(/\s+/).filter(Boolean);

/**
 * How well an entry matches: 0 not at all, 1 when the title starts with the
 * search, 0.8 when it holds it, 0.5 when the words are spread over the title
 * and keywords. Everything matches a blank search (1).
 */
export function paletteScore(search: string, title: string, keywords: readonly string[] = []) {
  const words = wordsOf(search);
  if (!words.length) return 1;
  const head = fold(title);
  const haystack = [head, ...keywords.map(fold)].join(' ');
  if (!words.every((w) => haystack.includes(w))) return 0;
  const q = words.join(' ');
  if (head.startsWith(q)) return 1;
  return head.includes(q) ? 0.8 : 0.5;
}

/**
 * The entries that match, best first (ties keep their order); all of them for
 * a blank search.
 */
export function rankEntries<T extends { title: string; subtitle?: string; keywords?: string[] }>(
  entries: readonly T[],
  search: string,
): T[] {
  return entries
    .map((e, i) => ({
      e,
      i,
      score: paletteScore(search, e.title, [e.subtitle ?? '', ...(e.keywords ?? [])]),
    }))
    .filter((x) => x.score > 0)
    .sort((a, b) => b.score - a.score || a.i - b.i)
    .map((x) => x.e);
}

/**
 * The best `limit` items whose name matches the search: names starting with it
 * first, then the rest, each in the list's own order (by book count, say).
 * Nothing for a blank search.
 */
export function topMatches<T>(
  items: readonly T[],
  nameOf: (item: T) => string,
  search: string,
  limit: number,
): T[] {
  if (!wordsOf(search).length) return [];
  return items
    .map((it, i) => ({ it, i, score: paletteScore(search, nameOf(it)) }))
    .filter((x) => x.score > 0)
    .sort((a, b) => Number(b.score === 1) - Number(a.score === 1) || a.i - b.i)
    .slice(0, limit)
    .map((x) => x.it);
}
