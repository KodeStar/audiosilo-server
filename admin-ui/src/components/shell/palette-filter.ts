/** Ids of entries offered for any search (a fallback), always ranked last. */
export const FALLBACK_PREFIX = 'fallback:';

/**
 * cmdk filter for the palette: every search word must appear in the entry's
 * title or keywords (case-insensitive substring), with a boost when the title
 * starts with the search. cmdk's default fuzzy scoring matches letters spread
 * across a whole subtitle, so "narrators" would match "Rescan Fiction" and
 * Enter would run the wrong command.
 */
export function paletteFilter(value: string, search: string, keywords?: string[]): number {
  const q = search.trim().toLowerCase();
  if (!q) return 1;
  // "Search all books for …" holds the search in its title by construction, so a
  // title match says nothing: it ranks below every real match.
  if (value.startsWith(FALLBACK_PREFIX)) return 0.01;
  const [title = '', ...rest] = (keywords ?? []).map((k) => k.toLowerCase());
  const haystack = [title, ...rest].join(' ');
  const words = q.split(/\s+/);
  if (!words.every((w) => haystack.includes(w))) return 0;
  if (title.startsWith(q)) return 1;
  if (title.includes(q)) return 0.8;
  return 0.5;
}

/** Whether every word of the search appears in the text (the palette's rule). */
export function matchesSearch(text: string, search: string): boolean {
  const hay = text.toLowerCase();
  return search
    .trim()
    .toLowerCase()
    .split(/\s+/)
    .filter(Boolean)
    .every((w) => hay.includes(w));
}

/**
 * The best `limit` items whose name matches the search: names starting with it
 * first, then the rest, each in the list's own order (by book count, say).
 */
export function topMatches<T>(
  items: readonly T[],
  nameOf: (item: T) => string,
  search: string,
  limit: number,
): T[] {
  const q = search.trim().toLowerCase();
  if (!q) return [];
  const hits = items.filter((it) => matchesSearch(nameOf(it), q));
  const prefix = hits.filter((it) => nameOf(it).toLowerCase().startsWith(q));
  const rest = hits.filter((it) => !nameOf(it).toLowerCase().startsWith(q));
  return [...prefix, ...rest].slice(0, limit);
}
