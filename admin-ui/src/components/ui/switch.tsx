import { Switch as SwitchPrimitive } from '@base-ui/react/switch';
import { cn } from '@/lib/utils';

// Shelf switch: on = brand pink, the thumb springs across (--ease-spring).
export function Switch({ className, ...props }: SwitchPrimitive.Root.Props) {
  return (
    <SwitchPrimitive.Root
      data-slot="switch"
      className={cn(
        'relative inline-flex h-6 w-[42px] shrink-0 cursor-pointer items-center rounded-full bg-border-strong p-[3px] transition-colors duration-(--dur-1) outline-none data-checked:bg-brand data-disabled:cursor-not-allowed data-disabled:opacity-45',
        className,
      )}
      {...props}
    >
      <SwitchPrimitive.Thumb className="size-[18px] rounded-full bg-white shadow-[0_1px_2px_rgb(0_0_0/0.25)] transition-transform duration-(--dur-2) ease-(--ease-spring) data-checked:translate-x-[18px]" />
    </SwitchPrimitive.Root>
  );
}
