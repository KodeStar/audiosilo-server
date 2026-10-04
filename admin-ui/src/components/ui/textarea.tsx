import * as React from 'react';
import { cn } from '@/lib/utils';
import { fieldClasses } from './field-classes';

// Shelf textarea: the Input's look (field-classes.ts), for multi-line values.
function Textarea({ className, ...props }: React.ComponentProps<'textarea'>) {
  return (
    <textarea
      data-slot="textarea"
      className={cn('min-h-[84px] px-3 py-2', fieldClasses, className)}
      {...props}
    />
  );
}

export { Textarea };
