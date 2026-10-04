import { useLayoutEffect, useRef } from 'react';
import { cn } from '@/lib/utils';

const inputClass =
  'w-full min-w-0 rounded-md border border-input bg-card outline-none focus-visible:border-ring focus-visible:shadow-[0_0_0_3px_color-mix(in_oklab,var(--ring)_20%,transparent)] aria-invalid:border-destructive';

/**
 * A value being edited in place (a book field, a chapter title): focused with
 * its text selected, Enter commits (Ctrl/Cmd+Enter when `multiline`, where
 * Enter is a new line), Escape cancels, leaving the field commits. `onDone`
 * gets the text, or undefined when cancelled, and whether the keyboard ended
 * it (Enter or Escape: focus can go back to what opened the editor).
 */
export function InlineEdit({
  initial,
  multiline = false,
  rows = 5,
  onDone,
  className,
  ...attrs
}: {
  initial: string;
  multiline?: boolean;
  rows?: number;
  onDone: (value: string | undefined, keyboard: boolean) => void;
  className?: string;
  id?: string;
} & React.AriaAttributes) {
  const ref = useRef<HTMLInputElement & HTMLTextAreaElement>(null);
  // Enter or Escape ends the edit before the blur that unmounting can fire.
  const ended = useRef(false);
  // Before paint, so a key pressed in the first frame isn't overwritten by select().
  useLayoutEffect(() => {
    ref.current?.focus();
    ref.current?.select();
  }, []);
  const end = (value: string | undefined, keyboard: boolean) => {
    if (ended.current) return;
    ended.current = true;
    onDone(value, keyboard);
  };
  const props = {
    ...attrs,
    ref,
    defaultValue: initial,
    onBlur: (e: React.FocusEvent<HTMLInputElement | HTMLTextAreaElement>) =>
      end(e.currentTarget.value, false),
    onKeyDown: (e: React.KeyboardEvent<HTMLInputElement | HTMLTextAreaElement>) => {
      if (e.key === 'Escape') {
        e.preventDefault();
        e.stopPropagation();
        end(undefined, true);
      } else if (e.key === 'Enter' && (!multiline || e.metaKey || e.ctrlKey)) {
        e.preventDefault();
        end(e.currentTarget.value, true);
      }
    },
  };
  return multiline ? (
    <textarea {...props} rows={rows} className={cn(inputClass, className)} />
  ) : (
    <input {...props} className={cn(inputClass, className)} />
  );
}
