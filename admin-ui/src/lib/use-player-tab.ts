import { useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { api } from '@/api/client';
import type { PairToken } from '@/api/types';
import { toastError } from '@/lib/errors';
import { toast } from '@/lib/toast';

/**
 * How long the token a player tab carries lives (POST /auth/pair ttl_seconds):
 * it rides the tab's URL and history, and is exchanged within seconds, so two
 * minutes rather than the server's ten.
 */
export const PLAYER_TAB_TTL_SECONDS = 120;

/**
 * Opens the web player in a new tab, signed in as this admin with no second
 * sign-in: a one-time pairing token per click, the tab pointed at `urlFor(token)`
 * once it is back. One token per click: a second click while one is on its way
 * is ignored (the ref sees it at once, even within one task; `busy` disables the
 * button). `failedTitle` heads the toast when the tab is blocked or the token
 * can't be made.
 */
export function usePlayerTab(failedTitle: string) {
  const { t } = useTranslation();
  const [busy, setBusy] = useState(false);
  const inFlight = useRef(false);
  const open = async (urlFor: (pairing: PairToken) => string) => {
    if (inFlight.current) return;
    // Opened in the click itself, or the browser takes it for a pop-up and blocks it;
    // pointed at the player once the token is back.
    const tab = window.open('', '_blank');
    if (!tab) {
      toast.add({ title: failedTitle, description: t('book.hero.listenBlocked'), type: 'error' });
      return;
    }
    tab.opener = null;
    inFlight.current = true;
    setBusy(true);
    try {
      tab.location.href = urlFor(await api.pairSelf(PLAYER_TAB_TTL_SECONDS));
    } catch (err) {
      tab.close();
      toastError(failedTitle, err);
    } finally {
      inFlight.current = false;
      setBusy(false);
    }
  };
  return { busy, open };
}
