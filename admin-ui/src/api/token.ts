import { readStorage, removeStorage, writeStorage } from '@/lib/storage';

// The admin session token. It keeps the key the classic console used, so an
// admin signed in before the redesign shipped stays signed in. It stays in
// localStorage rather than a cookie because the server's API is bearer-token only.

const TOKEN_KEY = 'audiosilo_token';

export const getToken = () => readStorage(TOKEN_KEY);
export const setToken = (token: string) => writeStorage(TOKEN_KEY, token);
export const clearToken = () => removeStorage(TOKEN_KEY);
