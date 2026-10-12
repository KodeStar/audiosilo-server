/**
 * A series name for comparison: no case, accents, punctuation or spacing. The
 * server's match.SeriesKey, by which it groups a series' spellings into one.
 */
export function seriesKey(name: string): string {
  return name
    .normalize('NFKD')
    .replace(/[^\p{L}\p{N}]/gu, '')
    .toLowerCase();
}

/**
 * Whether two names are one series, as the server groups them: the same
 * seriesKey, or (for a name with no letter or number, which is its own key
 * there) the same name.
 */
export function sameSeries(a: string, b: string): boolean {
  return a === b || (seriesKey(a) || a) === (seriesKey(b) || b);
}
