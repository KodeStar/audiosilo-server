import { useCallback, useMemo, useState } from 'react';
import type { AdminBook } from '@/api/types';
import { selectionKey } from './books-model';

/**
 * The books picked for a bulk action, keyed by identity (library + path). The
 * rows themselves are kept, so the dialogs can show current values and send
 * refs for books that have since scrolled out of the loaded pages.
 */
export function useSelection() {
  const [selected, setSelected] = useState<ReadonlyMap<string, AdminBook>>(() => new Map());

  const toggle = useCallback((b: AdminBook) => {
    setSelected((prev) => {
      const next = new Map(prev);
      const k = selectionKey(b);
      if (next.has(k)) next.delete(k);
      else next.set(k, b);
      return next;
    });
  }, []);

  /** Selects (or deselects) every book in `books`, leaving the rest as they are. */
  const setMany = useCallback((books: AdminBook[], on: boolean) => {
    setSelected((prev) => {
      const next = new Map(prev);
      for (const b of books) {
        if (on) next.set(selectionKey(b), b);
        else next.delete(selectionKey(b));
      }
      return next;
    });
  }, []);

  const clear = useCallback(() => setSelected(new Map()), []);
  const isSelected = useCallback((b: AdminBook) => selected.has(selectionKey(b)), [selected]);
  const books = useMemo(() => [...selected.values()], [selected]);

  return { size: selected.size, books, toggle, setMany, clear, isSelected };
}

export type Selection = ReturnType<typeof useSelection>;
