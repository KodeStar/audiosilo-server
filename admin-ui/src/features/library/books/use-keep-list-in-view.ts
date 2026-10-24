import { useCallback, useEffect, useLayoutEffect, useRef } from 'react';
import { deepEqual } from '@tanstack/react-router';
import { compact } from '@/lib/utils';
import type { LibrarySearch, Update } from '../library-search';

/**
 * How far `el` sits below its landing spot, the bottom of the sticky header,
 * which the element names as its scroll-margin-top. Negative when the page is
 * scrolled past it. (The page's scroll-padding-top, which keeps focused and
 * jumped-to elements clear of the header, plays no part: the landing is a
 * window.scrollTo, which ignores it.)
 */
export function offsetFromLanding(el: Element): number {
  const margin = parseFloat(getComputedStyle(el).scrollMarginTop) || 0;
  return el.getBoundingClientRect().top - margin;
}

/** Scrolls the page so `el` sits on its landing spot (the browser clamps to what the page can reach). */
function land(el: Element) {
  window.scrollTo({ top: window.scrollY + offsetFromLanding(el) });
}

// How long a landing that came up short waits for the page to grow to reach it.
const SETTLE_MS = 5000;
const SCROLL_KEYS = new Set(['PageUp', 'PageDown', 'Home', 'End', 'ArrowUp', 'ArrowDown', ' ']);

/** A key that scrolls the page: not one typed into a field. */
function scrollKey(e: KeyboardEvent): boolean {
  const t = e.target;
  const editable =
    t instanceof HTMLElement &&
    (t.isContentEditable || t.matches('input, textarea, select, [role="combobox"]'));
  return SCROLL_KEYS.has(e.key) && !editable;
}

/**
 * Lands `el` on its spot. When the page is too short to get there (the view
 * being shown is still loading, its placeholder shorter than the view), it
 * lands again each time the list's container (`el`'s parent) changes size,
 * until it gets there, the reader scrolls, or SETTLE_MS pass. Returns the stop.
 */
export function landAndSettle(el: Element): () => void {
  land(el);
  if (Math.abs(offsetFromLanding(el)) < 1) return () => {};
  let stopped = false;
  const onKey = (e: KeyboardEvent) => {
    if (scrollKey(e)) stop();
  };
  const ro = new ResizeObserver(() => {
    // A size change already queued when the watch stopped is not a reason to land.
    if (stopped) return;
    land(el);
    if (Math.abs(offsetFromLanding(el)) < 1) stop();
  });
  const timer = window.setTimeout(() => stop(), SETTLE_MS);
  function stop() {
    if (stopped) return;
    stopped = true;
    ro.disconnect();
    window.clearTimeout(timer);
    window.removeEventListener('wheel', stop);
    window.removeEventListener('touchstart', stop);
    window.removeEventListener('pointerdown', stop);
    window.removeEventListener('keydown', onKey);
  }
  window.addEventListener('wheel', stop, { passive: true });
  window.addEventListener('touchstart', stop, { passive: true });
  window.addEventListener('pointerdown', stop);
  window.addEventListener('keydown', onKey);
  ro.observe(el.parentElement ?? document.body);
  return stop;
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
  const settling = useRef<(() => void) | null>(null);
  const current = useRef(search);
  useLayoutEffect(() => {
    current.current = search;
    if (!pin.current || !anchor.current) return;
    pin.current = false;
    settling.current?.();
    settling.current = landAndSettle(anchor.current);
  }, [search]);
  useEffect(() => () => settling.current?.(), []);

  const keep = useCallback<Update>(
    (fn) => {
      // A change that changes nothing renders nothing, so it can't leave a pin behind.
      const prev = current.current;
      const el = anchor.current;
      pin.current =
        !!el && !deepEqual(compact(fn(prev)), compact(prev)) && offsetFromLanding(el) < 0;
      update(fn);
    },
    [update],
  );
  return { anchor, update: keep };
}
