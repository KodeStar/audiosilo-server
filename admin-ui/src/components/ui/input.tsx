import * as React from 'react';
import { Input as InputPrimitive } from '@base-ui/react/input';
import { cn } from '@/lib/utils';
import { fieldClasses } from './field-classes';

// Shelf input: 38px, radius 10, `--input` border; focus = ring border + a 3px
// 20% ring; invalid = red border + a message under the field.
function Input({ className, type, ...props }: React.ComponentProps<'input'>) {
  return (
    <InputPrimitive
      type={type}
      data-slot="input"
      className={cn('h-[38px] px-3', fieldClasses, className)}
      {...props}
    />
  );
}

export { Input };
