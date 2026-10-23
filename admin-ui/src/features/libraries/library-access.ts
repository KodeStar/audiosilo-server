import type { AdminModule, AdminUser, SetupEnvelope } from '@/api/types';

/**
 * Who "Give everyone access" grants a new library to: every enabled account
 * that isn't an admin (admins see every library) or a household member (a
 * member sees what their owner gives them), nor the demo account.
 */
export function everyoneToGrant(users: readonly AdminUser[]): AdminUser[] {
  return users.filter((u) => u.role !== 'admin' && !u.disabled && !u.managed_by && !u.is_demo);
}

/**
 * Whether the config declares this library (config.yaml, or the app that runs
 * the server; GET /admin/setup's config_libraries): its name and folder are
 * set there.
 */
export function configLibrary(setup: SetupEnvelope | undefined, name: string): boolean {
  return !!setup?.config_libraries.includes(name);
}

/**
 * The Kind field's hint, naming what a podcast library still needs (ADMIN-16):
 * the Podcasts module, a Bridge that supports podcasts, both, or nothing. The
 * module list not loaded (or no podcasts module) gives the general hint.
 */
export function podcastsHintKey(m: AdminModule | undefined): string {
  if (!m) return 'libraries.form.kindHint';
  const bridge = m.bridge_state === undefined || m.bridge_state === 'ready';
  if (m.enabled && bridge) return 'libraries.form.kindHintReady';
  if (m.enabled) return 'libraries.form.kindHintBridge';
  return bridge ? 'libraries.form.kindHintModule' : 'libraries.form.kindHint';
}
