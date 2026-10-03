import { Button as ButtonPrimitive } from '@base-ui/react/button';
import { cva, type VariantProps } from 'class-variance-authority';
import { cn } from '@/lib/utils';

// Shelf button (STYLEGUIDE.md section 8): ink `default`, pink `brand` only for the
// one key confirm on a view, hairline `outline`. Labels are verb + object.
const buttonVariants = cva(
  "inline-flex shrink-0 items-center justify-center gap-[7px] rounded-md border border-transparent text-[13.5px] font-semibold whitespace-nowrap transition-[background-color,border-color,color,transform] duration-(--dur-1) outline-none select-none active:translate-y-px disabled:pointer-events-none disabled:opacity-45 aria-disabled:pointer-events-none aria-disabled:opacity-45 [&_svg]:pointer-events-none [&_svg]:shrink-0 [&_svg:not([class*='size-'])]:size-4",
  {
    variants: {
      variant: {
        default:
          'bg-primary text-primary-foreground hover:bg-[color-mix(in_oklab,var(--primary)_88%,var(--brand))]',
        brand:
          'bg-brand text-brand-foreground hover:bg-[color-mix(in_oklab,var(--brand)_88%,black)]',
        outline: 'border-border-strong bg-card text-foreground hover:bg-accent',
        secondary:
          'bg-secondary text-secondary-foreground hover:bg-[color-mix(in_oklab,var(--secondary)_85%,var(--foreground))]',
        ghost: 'text-foreground hover:bg-accent aria-expanded:bg-accent',
        destructive:
          'bg-destructive text-white hover:bg-[color-mix(in_oklab,var(--destructive)_88%,black)]',
        'destructive-outline':
          'border-[color-mix(in_oklab,var(--destructive)_40%,transparent)] text-destructive hover:bg-destructive-soft',
        link: 'h-auto px-0 text-brand-ink hover:underline hover:underline-offset-3',
      },
      size: {
        default: 'h-9 px-3.5',
        sm: 'h-[30px] rounded-sm px-2.5 text-[12.5px]',
        lg: 'h-11 rounded-[12px] px-5 text-[15px]',
        icon: 'size-9',
        'icon-sm': 'size-[30px] rounded-sm',
      },
    },
    defaultVariants: {
      variant: 'default',
      size: 'default',
    },
  },
);

function Button({
  className,
  variant = 'default',
  size = 'default',
  ...props
}: ButtonPrimitive.Props & VariantProps<typeof buttonVariants>) {
  return (
    <ButtonPrimitive
      data-slot="button"
      className={cn(buttonVariants({ variant, size, className }))}
      {...props}
    />
  );
}

// eslint-disable-next-line react-refresh/only-export-components -- shadcn convention: links styled as buttons reuse the variants
export { Button, buttonVariants };
