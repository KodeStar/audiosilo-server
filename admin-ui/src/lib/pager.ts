/** One slot of a pager: a page number, or a gap of skipped pages. */
export type PagerItem = number | 'gap';

/**
 * The page numbers a pager shows for `page` of `count`: the first and the last,
 * and the pages either side of the current one; a run of skipped pages
 * is one gap, unless it is a single page, which is shown instead (a gap would
 * take its place anyway). None for a single page.
 */
export function pagerItems(page: number, count: number): PagerItem[] {
  if (count <= 1) return [];
  const keep = new Set([1, count]);
  for (const p of [page - 1, page, page + 1]) {
    if (p >= 1 && p <= count) keep.add(p);
  }
  const out: PagerItem[] = [];
  let prev = 0;
  for (const p of [...keep].sort((a, b) => a - b)) {
    if (p - prev === 2) out.push(prev + 1);
    else if (p - prev > 2) out.push('gap');
    out.push(p);
    prev = p;
  }
  return out;
}

/** How many pages `total` items fill at `size` a page (at least one). */
export function pageCount(total: number, size: number): number {
  return Math.max(1, Math.ceil(total / size));
}

/** `page` within 1..count (a page past the end, from an old link, is the last). */
export function clampPage(page: number | undefined, count: number): number {
  return Math.min(Math.max(1, page ?? 1), count);
}
