import { useBlocker } from '@tanstack/react-router';
import { TriangleAlert } from 'lucide-react';
import { Button } from '@/components/ui/button';
import { Dialog, DialogContent, DialogFooter } from '@/components/ui/dialog';

/** Unsaved edits: ask before leaving the page (and the browser's own prompt before unloading). */
export function LeaveGuard({
  dirty,
  title,
  description,
  stay,
  discard,
}: {
  dirty: boolean;
  title: string;
  description: string;
  stay: string;
  discard: string;
}) {
  const blocker = useBlocker({
    shouldBlockFn: () => true,
    disabled: !dirty,
    enableBeforeUnload: dirty,
    withResolver: true,
  });
  return (
    <Dialog
      open={blocker.status === 'blocked'}
      onOpenChange={(open) => (open ? undefined : blocker.reset?.())}
    >
      <DialogContent icon={TriangleAlert} tone="danger" title={title} description={description}>
        <DialogFooter>
          <Button variant="ghost" onClick={() => blocker.reset?.()}>
            {stay}
          </Button>
          <Button variant="destructive" onClick={() => blocker.proceed?.()}>
            {discard}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
