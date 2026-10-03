import { clearToken, getToken } from './token';
import type {
  AdminSettings,
  AdminStats,
  ErrorEnvelope,
  LoginResponse,
  ServerInfo,
  User,
} from './types';

const API = '/api/v1';

/** A failed API call: the HTTP status plus the server's {"error"} message. */
export class ApiError extends Error {
  readonly status: number;
  constructor(status: number, message: string) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
  }
}

let onUnauthorized: () => void = () => {};

/**
 * Registers what a 401 means for the app (drop the session, show sign-in). The
 * client clears the stored token itself before calling it.
 */
export function setUnauthorizedHandler(fn: () => void) {
  onUnauthorized = fn;
}

const LOGIN_PATH = '/auth/login';

/**
 * One API call. `explicitToken` authenticates with a token that is not the
 * stored session (signing a non-admin straight back out) and leaves the stored
 * session alone on a 401.
 */
async function request<T>(
  method: string,
  path: string,
  body?: unknown,
  explicitToken?: string,
): Promise<T> {
  const headers: Record<string, string> = {};
  const token = explicitToken ?? getToken();
  if (token) headers.Authorization = `Bearer ${token}`;
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  const res = await fetch(API + path, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  // Any 401 but a failed sign-in ends the session, including one sent with no
  // token at all: another tab (or the classic console) signed out and cleared it.
  if (res.status === 401 && path !== LOGIN_PATH && explicitToken === undefined) {
    clearToken();
    onUnauthorized();
  }
  const text = await res.text();
  let data: unknown = undefined;
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      // a non-JSON body (a proxy error page): fall back to the status text below
    }
  }
  if (!res.ok) {
    const envelope = data as Partial<ErrorEnvelope> | undefined;
    const msg =
      typeof envelope?.error === 'string' && envelope.error
        ? envelope.error
        : res.statusText || `HTTP ${res.status}`;
    throw new ApiError(res.status, msg);
  }
  return data as T;
}

export const api = {
  serverInfo: () => request<ServerInfo>('GET', '/server'),
  login: (username: string, password: string) =>
    request<LoginResponse>('POST', LOGIN_PATH, {
      username,
      password,
      device_name: 'admin-web',
    }),
  /** Revokes the stored session, or `token` when given (never touching storage). */
  logout: (token?: string) => request<void>('POST', '/auth/logout', undefined, token),
  me: () => request<User>('GET', '/me'),
  stats: () => request<AdminStats>('GET', '/admin/stats'),
  settings: () => request<AdminSettings>('GET', '/admin/settings'),
  scanLibrary: (id: number) => request<{ status: string }>('POST', `/admin/libraries/${id}/scan`),
};

/**
 * A book's cover image URL. <img> can't send headers, so media GETs carry the
 * session token as ?token= (the server accepts it only on media routes).
 */
export function coverUrl(libraryId: number, path: string): string {
  const q = new URLSearchParams({ path });
  const token = getToken();
  if (token) q.set('token', token);
  return `${API}/libraries/${libraryId}/cover?${q.toString()}`;
}
