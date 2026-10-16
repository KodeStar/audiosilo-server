import { useLayoutEffect, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { X } from 'lucide-react';
import { Badge } from '@/components/ui/badge';
import { fieldClasses } from '@/components/ui/field-classes';
import { cleanLabels } from '@/lib/labels';
import { cn } from '@/lib/utils';

/**
 * One label as a chip (a secondary pill, 24px; STYLEGUIDE.md "Field row"): with
 * `onRemove`, an x that takes it out of its list.
 */
export function LabelChip({
  label,
  onRemove,
  disabled,
}: {
  label: string;
  onRemove?: () => void;
  disabled?: boolean;
}) {
  const { t } = useTranslation();
  return (
    <Badge
      className={cn('h-[24px] max-w-full px-2.5 text-[12.5px] font-[550]', onRemove && 'pr-1')}
    >
      <span className="truncate">{label}</span>
      {onRemove ? (
        <button
          type="button"
          disabled={disabled}
          // A click keeps the focus where it is: Safari and Firefox on a Mac never
          // focus a clicked button, so the box's input would blur with nothing
          // focused in it and end an in-place edit before the click removed the chip.
          onMouseDown={(e) => e.preventDefault()}
          onClick={onRemove}
          aria-label={t('labels.remove', { label })}
          className="grid size-5 shrink-0 place-items-center rounded-full text-muted-foreground hover:bg-accent hover:text-foreground"
        >
          <X aria-hidden="true" />
        </button>
      ) : null}
    </Badge>
  );
}

/**
 * A list of labels (genres, tags) edited as chips: type a label and press Enter
 * or a comma to add it (a pasted "a, b" adds both), Backspace in the empty box
 * takes the last one back, a chip's x removes it. A label already in the list
 * (any case) isn't added twice. Controlled: `value` is the list, `onChange` gets
 * the next one; leaving the box adds what is typed. With `onDone` it is an
 * in-place editor (the Details card):
 * focused on open, Enter in the empty box or leaving it commits (whatever is
 * typed is added first), Escape cancels (`onDone(undefined, true)`), and
 * `keyboard` says whether a key ended it, so focus can go back to the value.
 */
export function LabelInput({
  id,
  value,
  onChange,
  onDone,
  placeholder,
  disabled,
  autoFocus,
  className,
  ...aria
}: {
  id?: string;
  value: string[];
  onChange: (next: string[]) => void;
  onDone?: (value: string[] | undefined, keyboard: boolean) => void;
  placeholder?: string;
  disabled?: boolean;
  autoFocus?: boolean;
  className?: string;
} & React.AriaAttributes) {
  const [text, setText] = useState('');
  const input = useRef<HTMLInputElement>(null);
  const box = useRef<HTMLDivElement>(null);
  const ended = useRef(false);

  useLayoutEffect(() => {
    if (autoFocus) input.current?.focus();
  }, [autoFocus]);

  /** value with the labels typed in `raw` (comma-separated) added. */
  const withTyped = (raw: string): string[] => cleanLabels([...value, ...raw.split(',')]).labels;

  const addTyped = (raw: string) => {
    const next = withTyped(raw);
    setText('');
    if (next.length !== value.length) onChange(next);
    return next;
  };

  const end = (next: string[] | undefined, keyboard: boolean) => {
    if (!onDone || ended.current) return;
    ended.current = true;
    onDone(next, keyboard);
  };

  const remove = (label: string) => {
    onChange(value.filter((l) => l !== label));
    input.current?.focus();
  };

  return (
    <div
      ref={box}
      className={cn(
        fieldClasses,
        'flex min-h-[38px] flex-wrap items-center gap-1.5 px-2 py-1.5 focus-within:border-ring focus-within:shadow-[0_0_0_3px_color-mix(in_oklab,var(--ring)_20%,transparent)]',
        disabled && 'bg-muted',
        className,
      )}
      onBlur={(e) => {
        // Leaving the whole box (not moving to one of its chips' buttons) commits;
        // without onDone (the bulk edit's boxes) it adds what is typed, so a label
        // left unentered isn't dropped by the form's submit.
        if (box.current?.contains(e.relatedTarget as Node | null)) return;
        if (onDone) end(withTyped(text), false);
        else if (text.trim()) addTyped(text);
      }}
    >
      {value.map((label) => (
        <LabelChip key={label} label={label} disabled={disabled} onRemove={() => remove(label)} />
      ))}
      <input
        {...aria}
        id={id}
        ref={input}
        value={text}
        disabled={disabled}
        autoComplete="off"
        placeholder={value.length ? undefined : placeholder}
        className="h-[26px] min-w-[8ch] flex-1 bg-transparent px-1 text-sm outline-none placeholder:text-subtle-foreground"
        onChange={(e) => {
          const v = e.target.value;
          // A comma ends a label (a pasted list adds every label in it).
          if (v.includes(',')) addTyped(v);
          else setText(v);
        }}
        onKeyDown={(e) => {
          if (e.key === 'Enter') {
            e.preventDefault();
            if (text.trim()) addTyped(text);
            else end(value, true);
          } else if (e.key === 'Escape' && onDone) {
            e.preventDefault();
            e.stopPropagation();
            end(undefined, true);
          } else if (e.key === 'Backspace' && !text && value.length) {
            e.preventDefault();
            setText(value[value.length - 1]);
            onChange(value.slice(0, -1));
          }
        }}
      />
    </div>
  );
}
