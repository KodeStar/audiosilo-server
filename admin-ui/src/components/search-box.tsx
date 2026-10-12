import { useEffect, useRef, useState } from 'react';
import { Search, X } from 'lucide-react';
import { Input } from '@/components/ui/input';
import { useDebounced } from '@/lib/use-debounced';
import { cn } from '@/lib/utils';

/** How long the search box waits for typing to pause before it searches. */
const SEARCH_DEBOUNCE_MS = 250;

/**
 * A search box for a list whose search lives in the URL (Books, Series): types
 * freely, tells `onChange` once typing pauses, with a button to clear it. A
 * change from outside (a chip's clear, Back) replaces what is typed.
 */
export function SearchBox({
  value,
  onChange,
  label,
  placeholder,
  clearLabel,
  className,
}: {
  value: string;
  onChange: (q: string) => void;
  label: string;
  placeholder: string;
  clearLabel: string;
  className?: string;
}) {
  const [text, setText] = useState(value);
  const settled = useDebounced(text.trim(), SEARCH_DEBOUNCE_MS);
  // The last value this box sent (or was given), to tell outside changes apart.
  const sent = useRef(value);
  const latest = useRef(onChange);
  useEffect(() => {
    latest.current = onChange;
  });

  useEffect(() => {
    if (value === sent.current) return;
    sent.current = value;
    setText(value);
  }, [value]);

  // Only once typing has paused (the debounced value caught up with the box), so
  // a change from outside isn't answered with the stale text from before it.
  useEffect(() => {
    if (settled !== text.trim() || settled === sent.current.trim()) return;
    sent.current = settled;
    latest.current(settled);
  }, [settled, text]);

  return (
    <label className={cn('relative flex items-center', className)}>
      <Search
        className="pointer-events-none absolute left-3 size-4 text-muted-foreground"
        aria-hidden="true"
      />
      <Input
        value={text}
        onChange={(e) => setText(e.target.value)}
        aria-label={label}
        placeholder={placeholder}
        autoComplete="off"
        className="pr-9 pl-9"
      />
      {text ? (
        <button
          type="button"
          aria-label={clearLabel}
          onClick={() => {
            setText('');
            sent.current = '';
            latest.current('');
          }}
          className="absolute right-1.5 grid size-7 place-items-center rounded-sm text-muted-foreground hover:bg-accent hover:text-foreground"
        >
          <X className="size-4" aria-hidden="true" />
        </button>
      ) : null}
    </label>
  );
}
