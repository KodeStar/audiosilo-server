import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { ApiError, api, setForbiddenHandler, setUnauthorizedHandler } from '@/api/client';
import { clearThumbCache } from '@/api/thumb-cache';
import { clearToken, getToken, setToken } from '@/api/token';
import type { LoginResponse } from '@/api/types';
import type { OIDCReturn } from './oidc-return';
import { SessionContext, type Session, type SessionState, type SignedOutReason } from './session';

/** The pause before a single sign-on's finish is asked once more after the server failed it. */
const OIDC_FINISH_RETRY_MS = 500;

/**
 * Finishes the single sign-on of `flow`, once more after a pause when the
 * server failed it (a 5xx: the store failed, so nothing was spent and the
 * sign-in's cookie stays for the rest of its minute, handlers_oidc.go). A
 * refusal (4xx) or a network failure, which may have spent it, is final.
 */
async function finishOIDC(flow: string | undefined): Promise<LoginResponse> {
  try {
    return await api.oidcFinish(flow);
  } catch (err) {
    if (!(err instanceof ApiError) || err.status < 500) throw err;
    await new Promise((resolve) => setTimeout(resolve, OIDC_FINISH_RETRY_MS));
    return api.oidcFinish(flow);
  }
}

/**
 * The console's session: the stored token checked once on load, or, after a
 * single sign-on (`oidcReturn`: main.tsx found /admin/#oidc=done), the sign-in the
 * server holds in its flow's cookie finished for a new session.
 */
export function SessionProvider({
  oidcReturn,
  children,
}: {
  oidcReturn: OIDCReturn | null;
  children: React.ReactNode;
}) {
  const qc = useQueryClient();
  const oidcDone = oidcReturn !== null;
  const oidcFlow = oidcReturn?.flow;
  const [state, setState] = useState<SessionState>(() =>
    oidcDone || getToken() ? { status: 'checking' } : { status: 'signed-out', reason: null },
  );
  // The finish runs once (the sign-in is single use), whatever the effect's re-runs.
  const finish = useRef<Promise<LoginResponse | null> | null>(null);
  // While a single sign-on is being finished, the stored token waits; one that
  // signs no admin in hands back to it (below).
  const [finishing, setFinishing] = useState(oidcDone);
  const [attempt, setAttempt] = useState(0);

  const dropSession = useCallback(
    (reason: SignedOutReason) => {
      clearToken();
      qc.clear();
      // The covers this admin saw stay with them, not with whoever signs in next.
      void clearThumbCache();
      setState({ status: 'signed-out', reason });
    },
    [qc],
  );

  // A 401 anywhere means the token is gone (expired, revoked, user disabled).
  useEffect(() => {
    setUnauthorizedHandler(() => dropSession('expired'));
  }, [dropSession]);

  // A 403 from an admin endpoint usually means another admin demoted this
  // account mid-session: re-check who is signed in (once at a time) and, if it
  // is no longer an admin, sign out with that reason instead of leaving every
  // screen showing "forbidden".
  useEffect(() => {
    let checking = false;
    setForbiddenHandler(() => {
      if (checking) return;
      checking = true;
      api
        .me()
        .then((user) => {
          if (user.role !== 'admin') dropSession('notAdmin');
        })
        .catch(() => {
          // a 401 is handled above; anything else leaves the session alone
        })
        .finally(() => {
          checking = false;
        });
    });
  }, [dropSession]);

  // Finish a single sign-on for a session. An account that isn't an admin is
  // signed straight out again (no session left behind), like the password
  // form; the session this browser held before is ended once the new one is
  // in. A finish that signs no admin in (none in this browser, as when someone
  // else's link to #oidc=done is followed; spent; a non-admin's) leaves a
  // stored session alone: it is checked as on any load.
  useEffect(() => {
    if (!oidcDone) return;
    let cancelled = false;
    const noAdmin = (reason: SignedOutReason) => {
      if (getToken()) setFinishing(false);
      else dropSession(reason);
    };
    finish.current ??= finishOIDC(oidcFlow).then(async (res) => {
      if (res.user.role === 'admin') return res;
      await api.logout(res.token).catch(() => {});
      return null;
    });
    finish.current
      .then((res) => {
        if (cancelled) return;
        if (!res) {
          noAdmin('notAdmin');
          return;
        }
        const previous = getToken();
        if (previous && previous !== res.token) void api.logout(previous).catch(() => {});
        setToken(res.token);
        setState({ status: 'signed-in', user: res.user });
      })
      .catch(() => {
        if (!cancelled) noAdmin('oidcFailed');
      });
    return () => {
      cancelled = true;
    };
  }, [oidcDone, oidcFlow, dropSession]);

  // Validate a stored token once on load (and on "Try again"); a single
  // sign-on's session replaces it instead, unless that signs no admin in.
  useEffect(() => {
    if (finishing || !getToken()) return;
    let cancelled = false;
    api
      .me()
      .then((user) => {
        if (cancelled) return;
        if (user.role !== 'admin') dropSession('notAdmin');
        else setState({ status: 'signed-in', user });
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        if (err instanceof ApiError && err.status === 401) dropSession('expired');
        else setState({ status: 'unreachable' });
      });
    return () => {
      cancelled = true;
    };
  }, [attempt, finishing, dropSession]);

  const value = useMemo<Session>(
    () => ({
      state,
      signIn: (token, user) => {
        setToken(token);
        setState({ status: 'signed-in', user });
      },
      updateUser: (user) => setState({ status: 'signed-in', user }),
      signOut: async () => {
        try {
          await api.logout();
        } catch {
          // the token is dropped locally either way
        }
        dropSession(null);
      },
      retry: () => {
        // The token can be gone by now (signed out in another tab); the check
        // effect skips without one, which would leave "checking" up for good.
        if (!getToken()) {
          dropSession('expired');
          return;
        }
        setState({ status: 'checking' });
        setAttempt((n) => n + 1);
      },
    }),
    [state, dropSession],
  );

  return <SessionContext.Provider value={value}>{children}</SessionContext.Provider>;
}
