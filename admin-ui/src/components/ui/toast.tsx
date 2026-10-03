import { Toast as ToastPrimitive } from '@base-ui/react/toast';
import {
  CircleCheckIcon,
  InfoIcon,
  Loader2Icon,
  OctagonXIcon,
  TriangleAlertIcon,
  XIcon,
} from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { toast } from '@/lib/toast';
import { cn } from '@/lib/utils';

// Shelf toast (STYLEGUIDE.md section 8): Base UI Toast, never sonner (it injects
// a <style> element, which the console's CSP blocks). Ink background, a small
// icon tile, title + one-line description, at most one action. Bottom right,
// above the mobile tab bar.

const ICONS = {
  success: CircleCheckIcon,
  info: InfoIcon,
  warning: TriangleAlertIcon,
  error: OctagonXIcon,
  loading: Loader2Icon,
} as const;

function ToastIcon({ type }: { type: string | undefined }) {
  const Icon = ICONS[(type ?? 'success') as keyof typeof ICONS] ?? CircleCheckIcon;
  return (
    <span
      data-slot="toast-icon"
      className="grid size-[22px] shrink-0 place-items-center rounded-[7px] bg-[color-mix(in_oklab,var(--primary-foreground)_14%,transparent)]"
    >
      <Icon aria-hidden="true" className={cn('size-3.5', type === 'loading' && 'animate-spin')} />
    </span>
  );
}

function ToastList() {
  const { toasts } = ToastPrimitive.useToastManager();
  const { t: tr } = useTranslation();
  return toasts.map((t) => (
    <ToastPrimitive.Root
      key={t.id}
      toast={t}
      data-slot="toast"
      className={cn(
        'absolute right-0 bottom-0 z-[calc(1000-var(--toast-index))] w-full origin-bottom rounded-lg bg-primary text-primary-foreground shadow-overlay select-none',
        '[--gap:0.75rem] [--height:var(--toast-frontmost-height,var(--toast-height))] [--peek:0.75rem] [--scale:calc(max(0,1-(var(--toast-index)*0.1)))] [--shrink:calc(1-var(--scale))] [--offset-y:calc(var(--toast-offset-y)*-1+calc(var(--toast-index)*var(--gap)*-1)+var(--toast-swipe-movement-y))]',
        'h-(--height) [transform:translateX(var(--toast-swipe-movement-x))_translateY(calc(var(--toast-swipe-movement-y)-(var(--toast-index)*var(--peek))-(var(--shrink)*var(--height))))_scale(var(--scale))] [transition:transform_var(--dur-3)_var(--ease-spring),opacity_var(--dur-3),height_150ms]',
        "after:absolute after:top-full after:left-0 after:h-[calc(var(--gap)+1px)] after:w-full after:content-['']",
        'data-expanded:h-(--toast-height) data-expanded:[transform:translateX(var(--toast-swipe-movement-x))_translateY(var(--offset-y))]',
        'data-limited:opacity-0 data-starting-style:[transform:translateY(150%)] [&[data-ending-style]:not([data-limited])]:[transform:translateY(150%)] data-ending-style:opacity-0',
      )}
    >
      <ToastPrimitive.Content className="flex h-full items-start gap-3 overflow-hidden py-3.5 pr-3.5 pl-4 transition-opacity duration-250 data-behind:opacity-0 data-expanded:opacity-100">
        <ToastIcon type={t.type} />
        <div className="flex min-w-0 flex-1 flex-col gap-px">
          <ToastPrimitive.Title className="text-[13.5px] font-[650]" />
          <ToastPrimitive.Description className="text-[12.5px] opacity-75" />
        </div>
        <ToastPrimitive.Action className="shrink-0 rounded-sm bg-[color-mix(in_oklab,var(--primary-foreground)_14%,transparent)] px-2.5 py-1 text-[12.5px] font-[650] whitespace-nowrap" />
        <ToastPrimitive.Close
          aria-label={tr('common.close')}
          className="grid size-6 shrink-0 place-items-center rounded-sm opacity-70 hover:opacity-100"
        >
          <XIcon aria-hidden="true" className="size-3.5" />
        </ToastPrimitive.Close>
      </ToastPrimitive.Content>
    </ToastPrimitive.Root>
  ));
}

export function Toaster({ children }: { children: React.ReactNode }) {
  return (
    <ToastPrimitive.Provider toastManager={toast}>
      {children}
      <ToastPrimitive.Portal>
        <ToastPrimitive.Viewport className="pointer-events-none fixed right-4 bottom-[84px] left-4 z-[100] mx-auto outline-none md:right-5 md:bottom-5 md:left-auto md:w-[380px] [&>*]:pointer-events-auto">
          <ToastList />
        </ToastPrimitive.Viewport>
      </ToastPrimitive.Portal>
    </ToastPrimitive.Provider>
  );
}
