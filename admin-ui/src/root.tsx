import { Outlet, useRouterState } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { RotateCw, Unplug } from 'lucide-react';
import { AppShell } from '@/components/shell/app-shell';
import { Button } from '@/components/ui/button';
import { LoginPage } from '@/features/auth/login-page';
import { isSetupPath } from '@/features/setup/setup-model';
import { useSetupRedirect } from '@/features/setup/use-setup-redirect';
import { useSession } from '@/lib/session';

/**
 * The sign-in gate. Every route renders through it, so a deep link survives
 * signing in: the URL stays put and the page appears once the session is valid.
 */
export function Root() {
  const { state, retry } = useSession();
  const { t } = useTranslation();

  if (state.status === 'checking') {
    return (
      <div
        className="grid min-h-dvh place-items-center"
        role="status"
        aria-label={t('auth.checking')}
      >
        <span className="skel size-10 rounded-[9px]" />
      </div>
    );
  }
  if (state.status === 'unreachable') {
    return (
      <main className="grid min-h-dvh place-items-center px-4">
        <div className="flex max-w-md flex-col items-center gap-3 text-center">
          <span className="grid size-10 place-items-center rounded-[11px] bg-destructive-soft text-destructive">
            <Unplug className="size-5" aria-hidden="true" />
          </span>
          <h1 className="h2">{t('auth.unreachableTitle')}</h1>
          <p className="text-muted-foreground">{t('auth.unreachableBody')}</p>
          <Button variant="outline" onClick={retry}>
            <RotateCw aria-hidden="true" />
            {t('common.tryAgain')}
          </Button>
        </div>
      </main>
    );
  }
  if (state.status === 'signed-out') return <LoginPage reason={state.reason} />;
  return <SignedIn />;
}

/**
 * A signed-in admin's console: the shell around every page but the first-run
 * flow, which is a page of its own; and the flow's once-a-session redirect
 * while the server hasn't finished it.
 */
function SignedIn() {
  const onSetup = useRouterState({ select: (s) => isSetupPath(s.location.pathname) });
  return (
    <>
      <SetupRedirect />
      {onSetup ? <Outlet /> : <AppShell />}
    </>
  );
}

/** The redirect as a leaf, so its pathname subscription re-renders nothing else. */
function SetupRedirect() {
  useSetupRedirect();
  return null;
}
