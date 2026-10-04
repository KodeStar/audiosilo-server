import { Checkbox as CheckboxPrimitive } from '@base-ui/react/checkbox';
import { CheckIcon, MinusIcon } from 'lucide-react';
import { cn } from '@/lib/utils';

// Shelf checkbox: checked and indeterminate are brand pink (STYLEGUIDE.md
// section 8). Base UI supplies role="checkbox", aria-checked ("mixed" when
// indeterminate) and the keyboard model. Give it an aria-label when no visible
// label sits next to it (a tile's "Select <title>").
export function Checkbox({ className, ...props }: CheckboxPrimitive.Root.Props) {
  return (
    <CheckboxPrimitive.Root
      data-slot="checkbox"
      className={cn(
        'grid size-[18px] shrink-0 cursor-pointer place-items-center rounded-[6px] border-[1.5px] border-border-strong bg-card text-brand-foreground transition-colors duration-(--dur-1) outline-none data-checked:border-brand data-checked:bg-brand data-indeterminate:border-brand data-indeterminate:bg-brand data-disabled:cursor-not-allowed data-disabled:opacity-45',
        className,
      )}
      {...props}
    >
      <CheckboxPrimitive.Indicator
        className="grid place-items-center"
        render={(indicatorProps, state) => (
          <span {...indicatorProps}>
            {state.indeterminate ? (
              <MinusIcon className="size-3" strokeWidth={3} aria-hidden="true" />
            ) : (
              <CheckIcon className="size-3" strokeWidth={3} aria-hidden="true" />
            )}
          </span>
        )}
      />
    </CheckboxPrimitive.Root>
  );
}
