import { useCallback, useLayoutEffect, useRef } from 'react';
import { compact } from '@/lib/utils';
import type { LibrarySearch, Update } from '../library-search';

/**
 * Whether the page is scrolled past `el`: its top is above where
 * scrollIntoView puts it (under the sticky header, its scroll-margin-top).
 */
export function scrolledPast(el: Element | null): boolean {
  if (!el) return false;
  const margin = parseFloat(getComputedStyle(el).scrollMarginTop) || 0;
  return el.getBoundingClientRect().top < margin;
}

/** The same params, whatever order their keys are in. */
function sameSearch(a: LibrarySearch, b: LibrarySearch): boolean {
  const key = (s: LibrarySearch) =>
    JSON.stringify(Object.entries(compact(s)).sort(([x], [y]) => (x < y ? -1 : x > y ? 1 : 0)));
  return key(a) === key(b);
}

/**
 * Keeps a list's top in view across the list's own changes (the Books view
 * switch, sort, search and filters). `update` writes the search params as the
 * given one does; when the page was scrolled past the list's top (the element
 * `anchor` is put on, just above the list's toolbar), the change scrolls that
 * top back under the sticky header once it renders, rather than leaving the
 * reader part-way down a list that changed height. Scrolled above it, the page
 * stays where it is.
 */
export function useKeepListInView(update: Update, search: LibrarySearch) {
  const anchor = useRef<HTMLDivElement>(null);
  // Set by a change made while scrolled past the anchor, taken by the render it causes.
  const pin = useRef(false);
  const current = useRef(search);
  useLayoutEffect(() => {
    current.current = search;
    if (!pin.current) return;
    pin.current = false;
    anchor.current?.scrollIntoView({ block: 'start' });
  }, [search]);

  const keep = useCallback<Update>(
    (fn) => {
      // A change that changes nothing renders nothing, so it can't leave a pin behind.
      const prev = current.current;
      pin.current = scrolledPast(anchor.current) && !sameSearch(fn(prev), prev);
      update(fn);
    },
    [update],
  );
  return { anchor, update: keep };
}
