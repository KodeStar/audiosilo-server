// localStorage that never throws: it can be unavailable (private mode, blocked
// storage), and every caller just wants "no value" / "not saved" then.

export function readStorage(key: string): string | null {
  try {
    return localStorage.getItem(key);
  } catch {
    return null;
  }
}

export function writeStorage(key: string, value: string) {
  try {
    localStorage.setItem(key, value);
  } catch {
    // not persisted: the value lasts for this page only
  }
}

export function removeStorage(key: string) {
  try {
    localStorage.removeItem(key);
  } catch {
    // nothing stored
  }
}
