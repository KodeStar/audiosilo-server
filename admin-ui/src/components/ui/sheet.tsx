import { Drawer } from '@base-ui/react/drawer';
import { XIcon } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { cn } from '@/lib/utils';

// Shelf sheet (STYLEGUIDE.md "Dialog / Sheet"): a 420px panel from the right for
// faceted filters, full width on a phone. Base UI Drawer (never vaul, which is
// Radix-based) supplies the modal, focus trap and swipe-to-dismiss.
//
//   <Sheet open onOpenChange title="Filter books" footer={…}>…</Sheet>

export function Sheet({
  open,
  onOpenChange,
  title,
  description,
  footer,
  children,
  className,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: React.ReactNode;
  description?: React.ReactNode;
  footer?: React.ReactNode;
  children: React.ReactNode;
  className?: string;
}) {
  const { t } = useTranslation();
  return (
    <Drawer.Root open={open} onOpenChange={onOpenChange} swipeDirection="right">
      <Drawer.Portal>
        <Drawer.Backdrop className="fixed inset-0 z-80 bg-overlay transition-opacity duration-(--dur-3) data-ending-style:opacity-0 data-starting-style:opacity-0" />
        <Drawer.Viewport className="fixed inset-0 z-90 flex justify-end">
          <Drawer.Popup
            className={cn(
              'flex h-full w-full max-w-[420px] flex-col border-l bg-popover text-popover-foreground shadow-overlay outline-none transition-transform duration-(--dur-3) ease-(--ease-out) data-ending-style:translate-x-full data-starting-style:translate-x-full',
              className,
            )}
          >
            <div className="flex items-start gap-3 border-b px-[22px] pt-5 pb-3.5">
              <div className="flex min-w-0 flex-1 flex-col gap-1">
                <Drawer.Title className="font-display text-[19px] leading-tight font-[680] tracking-[-0.02em]">
                  {title}
                </Drawer.Title>
                {description ? (
                  <Drawer.Description className="text-[13px] text-muted-foreground">
                    {description}
                  </Drawer.Description>
                ) : null}
              </div>
              <Drawer.Close
                className="-mt-1 -mr-2 grid size-9 shrink-0 place-items-center rounded-md text-muted-foreground hover:bg-accent hover:text-foreground"
                aria-label={t('common.close')}
              >
                <XIcon className="size-4" aria-hidden="true" />
              </Drawer.Close>
            </div>
            <Drawer.Content className="min-h-0 flex-1 overflow-y-auto px-[22px] pt-2 pb-[22px]">
              {children}
            </Drawer.Content>
            {footer ? (
              <div className="flex flex-wrap items-center justify-between gap-2 border-t px-[22px] py-3.5 pb-[max(14px,env(safe-area-inset-bottom))]">
                {footer}
              </div>
            ) : null}
          </Drawer.Popup>
        </Drawer.Viewport>
      </Drawer.Portal>
    </Drawer.Root>
  );
}
