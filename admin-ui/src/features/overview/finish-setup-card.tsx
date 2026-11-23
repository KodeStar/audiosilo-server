import { useState } from 'react';
import { Link } from '@tanstack/react-router';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { ChevronRight, KeyRound, ListChecks } from 'lucide-react';
import { saveSetup, useLibraries, useSetup } from '@/api/hooks';
import { Notice } from '@/components/notice';
import { Button, buttonVariants } from '@/components/ui/button';
import { hasBookLibrary, showFinishCard, skippedSteps } from '@/features/setup/setup-model';
import { toastError } from '@/lib/errors';
import { useCurrentUser } from '@/lib/session';

/**
 * The Overview's "Finish setting up" card (FIRST-RUN-PLAN.md decision 4):
 * while the first-run flow is unfinished ("Finish later"), a link back into it;
 * after it, the steps still skipped, each a link back into it; until an admin
 * dismisses the card (for every admin of the server).
 */
export function FinishSetupCard() {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const me = useCurrentUser();
  const setup = useSetup().data;
  const state = setup?.state;
  const libraries = useLibraries();
  const [busy, setBusy] = useState(false);
  const hasLibraries = hasBookLibrary(libraries.data ?? []);
  // setup.skip finishes the flow and dismisses its card, but the admin who
  // still uses the password printed in the log needs a way to replace it.
  if (state?.finished && state.dismissed && setup?.printed_password) {
    return (
      <Notice
        tone="warn"
        icon={KeyRound}
        role="status"
        title={t('setup.account.printed')}
        actions={
          <Link
            to="/people/user/$userId"
            params={{ userId: String(me.id) }}
            search={{ tab: 'sign-in' }}
            className={buttonVariants({ variant: 'outline' })}
          >
            {t('user.signIn.change')}
          </Link>
        }
      >
        {t('setup.account.printedBody')}
      </Notice>
    );
  }
  if (!libraries.data || !showFinishCard(state, hasLibraries)) return null;
  const skipped = skippedSteps(state, hasLibraries);

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
        {state.finished
          ? t('setup.card.body', { count: skipped.length })
          : t('setup.card.unfinished')}
      </p>
      <ul className="-mx-2 flex flex-col">
        {state.finished ? null : (
          <li>
            <Link
              to="/setup"
              className="flex items-center justify-between gap-2 rounded-md px-2 py-2 font-semibold hover:bg-accent"
            >
              {t('setup.card.continue')}
              <ChevronRight className="size-4 text-muted-foreground" aria-hidden="true" />
            </Link>
          </li>
        )}
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
