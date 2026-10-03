import { readStorage, removeStorage, writeStorage } from '@/lib/storage';

// The admin session token. It shares the key the classic console used, so moving
// between the two consoles (behind AUDIOSILO_ADMIN_NEXT) never asks to sign in
// twice. It stays in localStorage rather than a cookie because the server's API
// is bearer-token only.

const TOKEN_KEY = 'audiosilo_token';

export const getToken = () => readStorage(TOKEN_KEY);
export const setToken = (token: string) => writeStorage(TOKEN_KEY, token);
export const clearToken = () => removeStorage(TOKEN_KEY);
