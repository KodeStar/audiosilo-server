/**
 * cmdk filter for the palette: every search word must appear in the entry's
 * title or keywords (case-insensitive substring), with a boost when the title
 * starts with the search. cmdk's default fuzzy scoring matches letters spread
 * across a whole subtitle, so "narrators" would match "Rescan Fiction" and
 * Enter would run the wrong command.
 */
export function paletteFilter(_value: string, search: string, keywords?: string[]): number {
  const q = search.trim().toLowerCase();
  if (!q) return 1;
  const [title = '', ...rest] = (keywords ?? []).map((k) => k.toLowerCase());
  const haystack = [title, ...rest].join(' ');
  const words = q.split(/\s+/);
  if (!words.every((w) => haystack.includes(w))) return 0;
  if (title.startsWith(q)) return 1;
  if (title.includes(q)) return 0.8;
  return 0.5;
}
