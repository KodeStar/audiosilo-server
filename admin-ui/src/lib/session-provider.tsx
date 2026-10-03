import { useCallback, useEffect, useMemo, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { ApiError, api, setUnauthorizedHandler } from '@/api/client';
import { clearToken, getToken, setToken } from '@/api/session';
import { SessionContext, type Session, type SessionState, type SignedOutReason } from './session';

export function SessionProvider({ children }: { children: React.ReactNode }) {
  const qc = useQueryClient();
  const [state, setState] = useState<SessionState>(() =>
    getToken() ? { status: 'checking' } : { status: 'signed-out', reason: null },
  );
  const [attempt, setAttempt] = useState(0);

  const dropSession = useCallback(
    (reason: SignedOutReason) => {
      clearToken();
      qc.clear();
      setState({ status: 'signed-out', reason });
    },
    [qc],
  );

  // A 401 anywhere means the token is gone (expired, revoked, user disabled).
  useEffect(() => {
    setUnauthorizedHandler(() => dropSession('expired'));
  }, [dropSession]);

  // Validate a stored token once on load (and on "Try again").
  useEffect(() => {
    if (!getToken()) return;
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
  }, [attempt, dropSession]);

  const value = useMemo<Session>(
    () => ({
      state,
      signIn: (token, user) => {
        setToken(token);
        setState({ status: 'signed-in', user });
      },
      signOut: async () => {
        try {
          await api.logout();
        } catch {
          // the token is dropped locally either way
        }
        dropSession(null);
      },
      retry: () => {
        setState({ status: 'checking' });
        setAttempt((n) => n + 1);
      },
    }),
    [state, dropSession],
  );

  return <SessionContext.Provider value={value}>{children}</SessionContext.Provider>;
}
