import * as React from 'react';
import { cn } from '@/lib/utils';

// Shelf textarea: the Input's border, radius, focus ring and invalid state, for
// multi-line values.
function Textarea({ className, ...props }: React.ComponentProps<'textarea'>) {
  return (
    <textarea
      data-slot="textarea"
      className={cn(
        'min-h-[84px] w-full min-w-0 rounded-md border border-input bg-card px-3 py-2 text-sm transition-[border-color,box-shadow] duration-(--dur-1) outline-none placeholder:text-subtle-foreground',
        'focus-visible:border-ring focus-visible:shadow-[0_0_0_3px_color-mix(in_oklab,var(--ring)_20%,transparent)] focus-visible:outline-none',
        'disabled:bg-muted disabled:text-muted-foreground',
        'aria-invalid:border-destructive aria-invalid:shadow-[0_0_0_3px_color-mix(in_oklab,var(--destructive)_15%,transparent)]',
        className,
      )}
      {...props}
    />
  );
}

export { Textarea };
