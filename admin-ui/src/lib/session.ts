import { createContext, useContext } from 'react';
import type { User } from '@/api/types';

/**
 * Why the sign-in screen is showing, when it isn't simply a fresh visit. Keys
 * into the i18n `auth.*` messages.
 */
export type SignedOutReason = 'expired' | 'notAdmin' | null;

export type SessionState =
  | { status: 'checking' }
  | { status: 'unreachable' }
  | { status: 'signed-out'; reason: SignedOutReason }
  | { status: 'signed-in'; user: User };

export interface Session {
  state: SessionState;
  signIn: (token: string, user: User) => void;
  signOut: () => Promise<void>;
  retry: () => void;
}

export const SessionContext = createContext<Session | null>(null);

export function useSession(): Session {
  const s = useContext(SessionContext);
  if (!s) throw new Error('useSession must be used inside <SessionProvider>');
  return s;
}

/** The signed-in admin. Only call from screens rendered behind the sign-in gate. */
export function useCurrentUser(): User {
  const { state } = useSession();
  if (state.status !== 'signed-in') throw new Error('useCurrentUser called while signed out');
  return state.user;
}
