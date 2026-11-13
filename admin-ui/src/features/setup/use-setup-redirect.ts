import { useEffect } from 'react';
import { useNavigate, useRouterState } from '@tanstack/react-router';
import { useSetup } from '@/api/hooks';
import { redirectDecision } from './setup-model';

/**
 * Set once this browser session's first page has decided: sent to the flow, or
 * left where it was.
 */
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
    // not kept past this page: the next page load decides again
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
    const decision = redirectDecision(state, pathname, redirectedThisSession());
    if (decision === 'none') return;
    // Either way the session is marked, so moving on to the Overview from a deep
    // link, or leaving the flow with "Finish later", never pulls the admin back.
    markRedirected();
    if (decision === 'go') void navigate({ to: '/setup', replace: true });
  }, [state, pathname, navigate]);
}
