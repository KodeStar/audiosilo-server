/** What GET /server reports as the name until an admin sets one (config.DefaultServerName). */
export const DEFAULT_SERVER_NAME = 'AudioSilo';

/**
 * How the console names this server: its name once an admin has given it one
 * (Settings > General), else `host`.
 */
export function serverLabel(name: string | undefined, host: string): string {
  const named = name?.trim();
  return named && named !== DEFAULT_SERVER_NAME ? named : host;
}
