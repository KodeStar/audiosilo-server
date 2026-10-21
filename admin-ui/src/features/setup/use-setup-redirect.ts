import { useEffect } from 'react';
import { useNavigate, useRouterState } from '@tanstack/react-router';
import { useSetup } from '@/api/hooks';
import { isSetupPath, shouldRedirect } from './setup-model';

/** Set once the console has sent this browser session to the flow. */
const REDIRECTED_KEY = 'audiosilo.setup.redirected';

function redirectedThisSession(): boolean {
  try {
    return sessionStorage.getItem(REDIRECTED_KEY) === '1';
  } catch {
    return false;
  }
}

function markRedirected() {
  try {
    sessionStorage.setItem(REDIRECTED_KEY, '1');
  } catch {
    // not kept: the next page load sends the admin to the flow again
  }
}

/**
 * Sends an admin to the first-run flow while the server hasn't finished it,
 * once per browser session (FIRST-RUN-PLAN.md decision 1): after that, "Finish
 * later" or any link out of the flow stays where it goes. Mounted once, for a
 * signed-in admin.
 */
export function useSetupRedirect() {
  const state = useSetup().data?.state;
  const pathname = useRouterState({ select: (s) => s.location.pathname });
  const navigate = useNavigate();
  useEffect(() => {
    // Opening the flow some other way (a link, typing the address) counts too:
    // leaving it with "Finish later" mustn't bring the admin straight back.
    if (state && !state.finished && isSetupPath(pathname)) markRedirected();
    if (!shouldRedirect(state, pathname, redirectedThisSession())) return;
    markRedirected();
    void navigate({ to: '/setup', replace: true });
  }, [state, pathname, navigate]);
}
