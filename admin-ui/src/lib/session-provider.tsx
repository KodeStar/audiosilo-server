import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { ApiError, api, setForbiddenHandler, setUnauthorizedHandler } from '@/api/client';
import { clearThumbCache } from '@/api/thumb-cache';
import { clearToken, getToken, setToken } from '@/api/token';
import type { LoginResponse } from '@/api/types';
import { SessionContext, type Session, type SessionState, type SignedOutReason } from './session';

/**
 * The console's session: the stored token checked once on load, or, after a
 * single sign-on, `oidcToken` (the 60 s single-use token it landed on, which
 * main.tsx took out of the address bar) traded for a new session.
 */
export function SessionProvider({
  oidcToken,
  children,
}: {
  oidcToken: string | null;
  children: React.ReactNode;
}) {
  const qc = useQueryClient();
  const [state, setState] = useState<SessionState>(() =>
    oidcToken || getToken() ? { status: 'checking' } : { status: 'signed-out', reason: null },
  );
  // The exchange runs once (the token is single use), whatever the effect's re-runs.
  const exchange = useRef<Promise<LoginResponse | null> | null>(null);
  // While a single sign-on's token is being traded, the stored token waits; one
  // that signs no admin in hands back to it (below).
  const [trading, setTrading] = useState(Boolean(oidcToken));
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

  // Trade a single sign-on's token for a session. An account that isn't an
  // admin is signed straight out again (no session left behind), like the
  // password form; the session this browser held before is ended once the new
  // one is in. A token that signs no admin in (spent, forged in a link, a
  // non-admin's) leaves a stored session alone: it is checked as on any load.
  useEffect(() => {
    if (!oidcToken) return;
    let cancelled = false;
    const noAdmin = (reason: SignedOutReason) => {
      if (getToken()) setTrading(false);
      else dropSession(reason);
    };
    exchange.current ??= api.exchange(oidcToken).then(async (res) => {
      if (res.user.role === 'admin') return res;
      await api.logout(res.token).catch(() => {});
      return null;
    });
    exchange.current
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
  }, [oidcToken, dropSession]);

  // Validate a stored token once on load (and on "Try again"); a single
  // sign-on's token replaces it instead, unless that signs no admin in.
  useEffect(() => {
    if (trading || !getToken()) return;
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
  }, [attempt, trading, dropSession]);

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
