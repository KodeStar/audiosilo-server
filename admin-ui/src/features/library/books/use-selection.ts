import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import type { AdminBook } from '@/api/types';
import { refKey } from '@/lib/book-route';

/**
 * The items from the last one clicked (`anchor`, its `key`) to `target` in
 * `order`, both ends included, in list order; just `target` when either isn't
 * in the list (a Shift-click's range, on Books, Health and Match review).
 */
export function rangeOf<T, K>(
  order: readonly T[],
  anchor: K | null | undefined,
  target: T,
  key: (item: T) => K,
): T[] {
  const k = key(target);
  const to = order.findIndex((x) => key(x) === k);
  const from = anchor == null ? -1 : order.findIndex((x) => key(x) === anchor);
  if (to < 0 || from < 0) return [target];
  return order.slice(Math.min(from, to), Math.max(from, to) + 1);
}

/** Whether an input event had Shift held (a Shift-click extends a selection). */
export function shiftHeld(e: Event | React.SyntheticEvent | undefined): boolean {
  return !!e && 'shiftKey' in e && !!e.shiftKey;
}

// A caller with no list to range over: one array, so `order` holds still.
const NO_ORDER: readonly AdminBook[] = [];

/**
 * A mousedown handler for a tile or row: while selecting, Shift-mousedown is
 * kept from starting a text selection across the list (the click still toggles
 * the range).
 */
export function keepShiftClickFromSelectingText(selecting: boolean) {
  return (e: React.MouseEvent) => {
    if (selecting && e.shiftKey) e.preventDefault();
  };
}

/**
 * The books picked for a bulk action, keyed by identity (library + path). The
 * rows themselves are kept, so the dialogs can show current values and send
 * refs for books that have since scrolled out of the loaded pages.
 *
 * `order` is the list as shown, for Shift-click: `toggle(book, true)` gives
 * every book from the last one clicked to this one the state this one takes.
 * `selecting` is true while books are selected or the Select toggle is on
 * (`setMode`), so a click on a tile or row picks rather than opens; clearing
 * the selection turns the toggle off too.
 */
export function useSelection(order: readonly AdminBook[] = NO_ORDER) {
  const [selected, setSelected] = useState<ReadonlyMap<string, AdminBook>>(() => new Map());
  const [mode, setMode] = useState(false);
  const anchor = useRef<string | null>(null);
  const shown = useRef(order);
  useEffect(() => {
    shown.current = order;
  }, [order]);

  const toggle = useCallback((b: AdminBook, shift = false) => {
    const k = refKey(b);
    const from = anchor.current;
    anchor.current = k;
    setSelected((prev) => {
      const next = new Map(prev);
      const on = !prev.has(k);
      for (const x of shift ? rangeOf(shown.current, from, b, refKey) : [b]) {
        if (on) next.set(refKey(x), x);
        else next.delete(refKey(x));
      }
      return next;
    });
  }, []);

  /** Selects (or deselects) every book in `books`, leaving the rest as they are. */
  const setMany = useCallback((books: readonly AdminBook[], on: boolean) => {
    setSelected((prev) => {
      const next = new Map(prev);
      for (const b of books) {
        if (on) next.set(refKey(b), b);
        else next.delete(refKey(b));
      }
      return next;
    });
  }, []);

  const clear = useCallback(() => {
    anchor.current = null;
    setMode(false);
    setSelected(new Map());
  }, []);
  const isSelected = useCallback((b: AdminBook) => selected.has(refKey(b)), [selected]);
  const books = useMemo(() => [...selected.values()], [selected]);

  return {
    size: selected.size,
    books,
    toggle,
    setMany,
    clear,
    isSelected,
    selecting: mode || selected.size > 0,
    setMode,
  };
}

export type Selection = ReturnType<typeof useSelection>;
