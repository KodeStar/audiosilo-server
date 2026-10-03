import { cn } from '@/lib/utils';

/** The page container every screen sits in: 1440px max, 24px gutters (16 on phones). */
export function Page({ children, className }: { children: React.ReactNode; className?: string }) {
  return (
    <div className={cn('mx-auto max-w-[1440px] px-4 pt-5 pb-[120px] md:px-6 md:pt-7', className)}>
      {children}
    </div>
  );
}

/** What a screen shows while its code loads: the page frame with a placeholder. */
export function PageSkeleton() {
  return (
    <Page>
      <div className="flex flex-col gap-4" role="status" aria-busy="true">
        <span className="skel h-9 w-56" />
        <span className="skel h-[180px] rounded-xl" />
      </div>
    </Page>
  );
}
