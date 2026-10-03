import { Tabs as TabsPrimitive } from '@base-ui/react/tabs';
import { cn } from '@/lib/utils';

// Underline tabs for detail pages (STYLEGUIDE.md: the sub bar uses the segmented
// variant; detail pages use underline). Counts sit in the trigger in subtle text.

export const Tabs = TabsPrimitive.Root;
export const TabsPanel = TabsPrimitive.Panel;

export function TabsList({ className, children, ...props }: TabsPrimitive.List.Props) {
  return (
    <TabsPrimitive.List
      className={cn('hscroll relative flex gap-5 border-b', className)}
      {...props}
    >
      {children}
      <TabsPrimitive.Indicator className="absolute bottom-[-1px] left-(--active-tab-left) h-0.5 w-(--active-tab-width) rounded-full bg-foreground transition-[left,width] duration-(--dur-2) ease-(--ease-out)" />
    </TabsPrimitive.List>
  );
}

export function TabsTab({
  className,
  count,
  children,
  ...props
}: TabsPrimitive.Tab.Props & { count?: number }) {
  return (
    <TabsPrimitive.Tab
      className={cn(
        'inline-flex h-10 shrink-0 cursor-pointer items-center gap-1.5 text-[13.5px] font-[550] whitespace-nowrap text-muted-foreground outline-none hover:text-foreground data-active:text-foreground',
        className,
      )}
      {...props}
    >
      {children}
      {count !== undefined ? (
        <span className="text-[12px] text-subtle-foreground tabular-nums">{count}</span>
      ) : null}
    </TabsPrimitive.Tab>
  );
}
