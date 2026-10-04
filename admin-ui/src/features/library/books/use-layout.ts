import { useEffect, useLayoutEffect, useState, type RefObject } from 'react';
import { useWindowVirtualizer } from '@tanstack/react-virtual';
import { useMediaQuery } from '@/lib/use-media-query';
import { shouldLoadMore } from './books-model';

// Layout readings the virtualized grid and table need: the list's width, its
// distance from the top of the document (the window virtualizer's scroll
// margin) and whether the viewport is a phone's. jsdom has no layout, so each
// starts from a value the components treat as "not measured yet".

/** The element's content width, kept current by a ResizeObserver (0 until measured). */
export function useWidth(ref: RefObject<HTMLElement | null>): number {
  const [width, setWidth] = useState(0);
  useLayoutEffect(() => {
    const el = ref.current;
    if (!el) return;
    setWidth(el.clientWidth);
    const ro = new ResizeObserver(([entry]) => {
      if (entry) setWidth(entry.contentRect.width);
    });
    ro.observe(el);
    return () => ro.disconnect();
  }, [ref]);
  return width;
}

/**
 * The element's offset from the top of the document, read again whenever the
 * page's size changes (shelves above the list load in and push it down), and
 * set only when it moved.
 */
export function useDocumentTop(ref: RefObject<HTMLElement | null>): number {
  const [top, setTop] = useState(0);
  useLayoutEffect(() => {
    const el = ref.current;
    if (!el) return;
    const read = () => {
      const next = Math.round(el.getBoundingClientRect().top + window.scrollY);
      setTop((prev) => (prev === next ? prev : next));
    };
    read();
    const ro = new ResizeObserver(read);
    ro.observe(document.body);
    return () => ro.disconnect();
  }, [ref]);
  return top;
}

/** Below the md breakpoint (721px), where the grid has two columns and tables stack. */
export function useIsPhone(): boolean {
  return useMediaQuery('(max-width: 720px)');
}

/** Before the first measurement (and in jsdom): a desktop-sized window. */
const INITIAL_RECT = { width: 1024, height: 900 };

/**
 * Rows of fixed height virtualized against the window scroll (the page scrolls,
 * not an inner box), from where `ref` starts in the document; nearing the end
 * (within `ahead` rows) loads the next page. Sizes come from `rowHeight`, so
 * nothing has to be measured.
 */
export function useWindowRows({
  ref,
  count,
  rowHeight,
  gap = 0,
  overscan,
  ahead,
  hasMore,
  loadingMore,
  onLoadMore,
}: {
  ref: RefObject<HTMLElement | null>;
  count: number;
  rowHeight: number;
  gap?: number;
  overscan: number;
  ahead?: number;
  hasMore: boolean;
  loadingMore: boolean;
  onLoadMore: () => void;
}) {
  const top = useDocumentTop(ref);
  const virtualizer = useWindowVirtualizer({
    count,
    estimateSize: () => rowHeight,
    gap,
    overscan,
    scrollMargin: top,
    initialRect: INITIAL_RECT,
    useFlushSync: false,
  });
  // A new row height (a resize, the phone layout) means new rows.
  useEffect(() => virtualizer.measure(), [virtualizer, rowHeight, gap]);

  const items = virtualizer.getVirtualItems();
  const last = items.at(-1)?.index;
  useEffect(() => {
    if (shouldLoadMore(last, count, hasMore, loadingMore, ahead)) onLoadMore();
  }, [last, count, hasMore, loadingMore, ahead, onLoadMore]);

  return { virtualizer, items, margin: virtualizer.options.scrollMargin };
}
