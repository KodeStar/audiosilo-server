import { Dialog as DialogPrimitive } from '@base-ui/react/dialog';
import { LoaderCircle, XIcon, type LucideIcon } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/button';
import { cn } from '@/lib/utils';

// Shelf dialog (STYLEGUIDE.md section 8): radius 20, a 40px tinted icon badge,
// title + one-line description, a scrolling body and a sticky footer. Below the
// md breakpoint it is a bottom sheet. Base UI Dialog supplies focus trapping,
// aria-modal and the labelled-by-title wiring.
//
//   <DialogContent title=… icon=…>
//     <DialogBody>…</DialogBody>
//     <DialogFooter>…buttons…</DialogFooter>
//   </DialogContent>
//
// A form wraps both in <form className="contents"> so its submit button can sit
// in the footer.
//
// A dialog that switches between two DialogContents (a form, then its result)
// switches back in the root's onOpenChangeComplete, never in onOpenChange: a
// DialogContent mounted while the root animates out is never unmounted.

const TONES = {
  brand: 'bg-brand-soft text-brand-ink',
  danger: 'bg-destructive-soft text-destructive',
  info: 'bg-info-soft text-info',
  success: 'bg-success-soft text-success',
  community: 'bg-prov-community-soft text-prov-community',
} as const;

export type DialogTone = keyof typeof TONES;

export const Dialog = DialogPrimitive.Root;

export function DialogContent({
  title,
  description,
  icon: Icon,
  tone = 'brand',
  size = 'default',
  children,
  className,
}: {
  title: React.ReactNode;
  description?: React.ReactNode;
  icon?: LucideIcon;
  tone?: DialogTone;
  size?: 'default' | 'lg';
  children?: React.ReactNode;
  className?: string;
}) {
  const { t } = useTranslation();
  return (
    <DialogPrimitive.Portal>
      <DialogPrimitive.Backdrop className="fixed inset-0 z-80 bg-overlay data-closed:animate-out data-closed:fade-out-0 data-open:animate-in data-open:fade-in-0" />
      <DialogPrimitive.Popup
        className={cn(
          'fixed z-90 flex flex-col border bg-popover text-popover-foreground shadow-overlay outline-none data-closed:animate-out data-closed:fade-out-0 data-open:animate-in data-open:fade-in-0',
          // phones: a bottom sheet
          'inset-x-0 bottom-0 max-h-[92dvh] rounded-t-2xl pb-[env(safe-area-inset-bottom)] data-open:slide-in-from-bottom-8',
          // md and up: centred
          'md:inset-x-auto md:top-1/2 md:bottom-auto md:left-1/2 md:max-h-[min(86dvh,860px)] md:w-[calc(100vw-48px)] md:-translate-x-1/2 md:-translate-y-1/2 md:rounded-2xl md:pb-0 md:data-open:slide-in-from-bottom-0 md:data-open:zoom-in-[.97]',
          size === 'lg' ? 'md:max-w-[760px]' : 'md:max-w-[520px]',
          className,
        )}
      >
        <div className="flex items-start gap-3.5 px-5 pt-5 pb-3 md:px-6 md:pt-6">
          {Icon ? (
            <span
              className={cn('grid size-10 shrink-0 place-items-center rounded-[12px]', TONES[tone])}
              aria-hidden="true"
            >
              <Icon className="size-5" />
            </span>
          ) : null}
          <div className="flex min-w-0 flex-1 flex-col gap-1 pt-0.5">
            <DialogPrimitive.Title className="font-display text-[19px] leading-tight font-[680] tracking-[-0.02em] [overflow-wrap:anywhere]">
              {title}
            </DialogPrimitive.Title>
            {description ? (
              <DialogPrimitive.Description className="text-[13.5px] text-muted-foreground">
                {description}
              </DialogPrimitive.Description>
            ) : null}
          </div>
          <DialogPrimitive.Close
            className="-mt-1 -mr-2 grid size-9 shrink-0 place-items-center rounded-md text-muted-foreground hover:bg-accent hover:text-foreground"
            aria-label={t('common.close')}
          >
            <XIcon className="size-4" aria-hidden="true" />
          </DialogPrimitive.Close>
        </div>
        {children}
      </DialogPrimitive.Popup>
    </DialogPrimitive.Portal>
  );
}

/** The dialog's scrolling middle. */
export function DialogBody({ className, ...props }: React.ComponentProps<'div'>) {
  return (
    <div
      className={cn('min-h-0 flex-1 overflow-y-auto px-5 pt-1 pb-5 md:px-6', className)}
      {...props}
    />
  );
}

/** The sticky footer: actions, primary last. Never a destructive action next to Save. */
export function DialogFooter({ className, ...props }: React.ComponentProps<'div'>) {
  return (
    <div
      className={cn(
        'flex flex-wrap items-center justify-end gap-2 border-t bg-muted/50 px-5 py-3.5 md:rounded-b-2xl md:px-6',
        className,
      )}
      {...props}
    />
  );
}

/** A footer button that closes the dialog (Cancel, Done). */
export const DialogClose = DialogPrimitive.Close;

/**
 * A form dialog's footer: Cancel, then the submit button, which spins while the
 * form is busy and keeps its label.
 */
export function DialogFormFooter({
  submitLabel,
  busy,
  disabled,
  variant = 'default',
}: {
  submitLabel: string;
  busy: boolean;
  disabled?: boolean;
  variant?: 'default' | 'destructive';
}) {
  const { t } = useTranslation();
  return (
    <DialogFooter>
      <DialogClose render={<Button type="button" variant="ghost" />}>
        {t('common.cancel')}
      </DialogClose>
      <Button type="submit" variant={variant} disabled={busy || disabled}>
        {busy ? <LoaderCircle className="animate-spin" aria-hidden="true" /> : null}
        {submitLabel}
      </Button>
    </DialogFooter>
  );
}
