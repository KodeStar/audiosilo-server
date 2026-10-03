import { cva, type VariantProps } from 'class-variance-authority';
import { cn } from '@/lib/utils';

// Shelf badge: a 22px pill. Status badges always carry a word (and usually an
// icon); colour is never the only signal.
const badgeVariants = cva(
  'inline-flex h-[22px] shrink-0 items-center gap-1 rounded-full px-2 text-[11.5px] font-semibold whitespace-nowrap [&_svg]:size-3 [&_svg]:shrink-0',
  {
    variants: {
      variant: {
        secondary: 'bg-secondary text-secondary-foreground',
        outline: 'text-muted-foreground shadow-[inset_0_0_0_1px_var(--border-strong)]',
        ink: 'bg-primary text-primary-foreground',
        success: 'bg-success-soft text-success',
        warning: 'bg-warning-soft text-warning',
        destructive: 'bg-destructive-soft text-destructive',
        info: 'bg-info-soft text-info',
        brand: 'bg-brand-soft text-brand-ink',
      },
    },
    defaultVariants: { variant: 'secondary' },
  },
);

export function Badge({
  className,
  variant,
  ...props
}: React.ComponentProps<'span'> & VariantProps<typeof badgeVariants>) {
  return (
    <span data-slot="badge" className={cn(badgeVariants({ variant }), className)} {...props} />
  );
}
