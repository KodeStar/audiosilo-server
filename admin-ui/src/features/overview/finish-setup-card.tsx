import { useState } from 'react';
import { Link } from '@tanstack/react-router';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { ChevronRight, ListChecks } from 'lucide-react';
import { saveSetup, useSetup } from '@/api/hooks';
import { Button } from '@/components/ui/button';
import { showFinishCard, skippedSteps } from '@/features/setup/setup-model';
import { toastError } from '@/lib/errors';

/**
 * The Overview's "Finish setting up" card (FIRST-RUN-PLAN.md decision 4):
 * after the first-run flow, the steps still skipped, each a link back into it,
 * until an admin dismisses the card (for every admin of the server).
 */
export function FinishSetupCard() {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const state = useSetup().data?.state;
  const [busy, setBusy] = useState(false);
  if (!state || !showFinishCard(state)) return null;
  const skipped = skippedSteps(state);

  const dismiss = async () => {
    setBusy(true);
    try {
      await saveSetup(qc, { dismissed: true });
    } catch (err) {
      toastError(t('setup.card.dismissFailed'), err);
    } finally {
      setBusy(false);
    }
  };

  return (
    <section
      className="flex flex-col gap-3 rounded-xl border bg-card p-5"
      aria-labelledby="finish-setup-heading"
    >
      <h3 id="finish-setup-heading" className="h3 flex items-center gap-2">
        <ListChecks className="size-[18px] text-muted-foreground" aria-hidden="true" />
        {t('setup.card.title')}
      </h3>
      <p className="text-[13.5px] text-muted-foreground">
        {t('setup.card.body', { count: skipped.length })}
      </p>
      <ul className="-mx-2 flex flex-col">
        {skipped.map((id) => (
          <li key={id}>
            <Link
              to="/setup"
              search={{ step: id }}
              className="flex items-center justify-between gap-2 rounded-md px-2 py-2 font-semibold hover:bg-accent"
            >
              {t(`setup.step.${id}.title`)}
              <ChevronRight className="size-4 text-muted-foreground" aria-hidden="true" />
            </Link>
          </li>
        ))}
      </ul>
      <div>
        <Button variant="ghost" size="sm" disabled={busy} onClick={() => void dismiss()}>
          {t('setup.card.dismiss')}
        </Button>
      </div>
    </section>
  );
}
