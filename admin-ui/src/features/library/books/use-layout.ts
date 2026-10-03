import { useEffect, useLayoutEffect, useState, type RefObject } from 'react';

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
 * The element's offset from the top of the document. Re-read after every render
 * (shelves above the list load in and push it down), set only when it moved.
 */
export function useDocumentTop(ref: RefObject<HTMLElement | null>): number {
  const [top, setTop] = useState(0);
  // eslint-disable-next-line react-hooks/exhaustive-deps -- every render on purpose; it settles once the offset holds still
  useLayoutEffect(() => {
    const el = ref.current;
    if (!el) return;
    const next = Math.round(el.getBoundingClientRect().top + window.scrollY);
    setTop((prev) => (prev === next ? prev : next));
  });
  return top;
}

const PHONE = '(max-width: 720px)';

/** Below the md breakpoint (721px), where the grid has two columns and tables stack. */
export function useIsPhone(): boolean {
  const [phone, setPhone] = useState(() => window.matchMedia(PHONE).matches);
  useEffect(() => {
    const mq = window.matchMedia(PHONE);
    const onChange = () => setPhone(mq.matches);
    mq.addEventListener('change', onChange);
    return () => mq.removeEventListener('change', onChange);
  }, []);
  return phone;
}
