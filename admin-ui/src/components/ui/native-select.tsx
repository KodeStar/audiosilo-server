import { ChevronDown } from 'lucide-react';
import { cn } from '@/lib/utils';

// A styled native <select> (shadcn's native-select): full keyboard and screen
// reader support for free, and the platform picker on phones.
export function NativeSelect({ className, ...props }: React.ComponentProps<'select'>) {
  return (
    <span className={cn('relative inline-flex w-full', className)}>
      <select
        data-slot="native-select"
        className="h-[38px] w-full min-w-0 cursor-pointer appearance-none rounded-md border border-input bg-card pr-9 pl-3 text-sm transition-[border-color,box-shadow] duration-(--dur-1) outline-none focus-visible:border-ring focus-visible:shadow-[0_0_0_3px_color-mix(in_oklab,var(--ring)_20%,transparent)] focus-visible:outline-none disabled:cursor-not-allowed disabled:bg-muted disabled:text-muted-foreground aria-invalid:border-destructive"
        {...props}
      />
      <ChevronDown
        className="pointer-events-none absolute top-1/2 right-3 size-4 -translate-y-1/2 text-muted-foreground"
        aria-hidden="true"
      />
    </span>
  );
}
