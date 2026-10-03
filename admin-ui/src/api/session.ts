// The admin session token. It shares the key the classic console used, so moving
// between the two consoles (behind AUDIOSILO_ADMIN_NEXT) never asks to sign in
// twice. It stays in localStorage rather than a cookie because the server's API
// is bearer-token only.

const TOKEN_KEY = 'audiosilo_token';

export function getToken(): string | null {
  try {
    return localStorage.getItem(TOKEN_KEY);
  } catch {
    return null;
  }
}

export function setToken(token: string) {
  try {
    localStorage.setItem(TOKEN_KEY, token);
  } catch {
    // storage unavailable: the session lasts until the tab closes
  }
}

export function clearToken() {
  try {
    localStorage.removeItem(TOKEN_KEY);
  } catch {
    // nothing stored
  }
}
