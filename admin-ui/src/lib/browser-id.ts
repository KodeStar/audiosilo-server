import { readStorage, writeStorage } from './storage';

// A random id this browser keeps for itself and sends with each sign-in
// (POST /auth/login's device_id), so the server announces a sign-in from a browser
// it has seen before only once, not at every sign-in (auth.IssueSession). It is not
// a secret and signs no one in: at most it keeps a "new device" notice quiet, and
// an admin signing the browser out from People > Devices makes the server forget it.
// It outlives sign-outs on purpose.

const KEY = 'audiosilo_browser_id';
const VALID = /^[A-Za-z0-9_-]{16,64}$/;

/** This browser's id, made on first use (undefined if the browser can't make one). */
export function browserId(): string | undefined {
  const stored = readStorage(KEY);
  if (stored && VALID.test(stored)) return stored;
  try {
    // getRandomValues, not randomUUID: the latter needs a secure context, and a
    // console on plain http (a LAN address) is common.
    const bytes = crypto.getRandomValues(new Uint8Array(16));
    const id = Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('');
    writeStorage(KEY, id);
    return id;
  } catch {
    return undefined;
  }
}
