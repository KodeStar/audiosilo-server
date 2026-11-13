import { useEffect } from 'react';
import { useNavigate, useRouterState } from '@tanstack/react-router';
import { useSetup } from '@/api/hooks';
import { shouldRedirect } from './setup-model';

/** Set once the console has sent this browser session to the flow. */
const REDIRECTED_KEY = 'audiosilo.setup.redirected';

/**
 * The mark for this page only, where sessionStorage can't be used (blocked site
 * data): else every navigation out of the flow would be sent straight back.
 */
let redirectedHere = false;

function redirectedThisSession(): boolean {
  try {
    return sessionStorage.getItem(REDIRECTED_KEY) === '1';
  } catch {
    return redirectedHere;
  }
}

function markRedirected() {
  try {
    sessionStorage.setItem(REDIRECTED_KEY, '1');
  } catch {
    // not kept past this page: the next page load sends the admin to the flow again
    redirectedHere = true;
  }
}

/**
 * Sends an admin to the first-run flow while the server hasn't finished it,
 * once per browser session and only from the Overview (FIRST-RUN-PLAN.md
 * decision 1, server#365): after that, "Finish later" or any link out of the
 * flow stays where it goes. Mounted once, for a signed-in admin.
 */
export function useSetupRedirect() {
  const state = useSetup().data?.state;
  const pathname = useRouterState({ select: (s) => s.location.pathname });
  const navigate = useNavigate();
  useEffect(() => {
    if (!state || state.finished) return;
    const go = shouldRedirect(state, pathname, redirectedThisSession());
    // The first page the session shows with the state loaded decides: the
    // Overview goes to the flow, any other page (a deep link such as the flow's
    // own "Open the import" in a new tab, or the flow itself) stays. Either way
    // the session is marked, so moving on to the Overview from there, or
    // leaving the flow with "Finish later", never pulls the admin back.
    markRedirected();
    if (go) void navigate({ to: '/setup', replace: true });
  }, [state, pathname, navigate]);
}
