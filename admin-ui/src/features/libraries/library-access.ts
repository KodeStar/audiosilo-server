import type { AdminLibrary, AdminModule, AdminUser } from '@/api/types';
import { separatorOf } from '@/lib/paths';

/**
 * Who "Give everyone access" grants a new library to: every enabled account
 * that isn't an admin (admins see every library) or a household member (a
 * member sees what their owner gives them), nor the demo account.
 */
export function everyoneToGrant(users: readonly AdminUser[]): AdminUser[] {
  return users.filter((u) => u.role !== 'admin' && !u.disabled && !u.managed_by && !u.is_demo);
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

/** How a folder sits against another books library's: its folder, inside it, or holding it. */
export interface RootOverlap {
  name: string;
  how: 'same' | 'inside' | 'holds';
}

/**
 * A server path near enough as the server cleans it to compare two: a drive
 * path's "/" as "\\", no doubled separators and none trailing ("/" and "C:\\"
 * stay as they are).
 */
const cleanRoot = (p: string) => {
  const sep = separatorOf(p.trim());
  const one = p.trim().replace(sep === '\\' ? /[\\/]+/g : /\/+/g, sep);
  const trimmed = one.replace(/[\\/]+$/, '');
  return trimmed === '' || /^[A-Za-z]:$/.test(trimmed) ? one : trimmed;
};

/**
 * The first books library (other than `editingId`) whose folder is `folder`,
 * holds it or sits inside it (ADMIN-14). The server allows a books library
 * there, but the books under both folders are then listed twice, once in each
 * library, so the console asks first. Podcast libraries are left out: the
 * server refuses any overlap with one (409 root_overlaps). Paths compare as
 * the server's do, once cleaned: case and all.
 */
export function bookRootOverlap(
  folder: string,
  libraries: readonly Pick<AdminLibrary, 'id' | 'name' | 'root' | 'kind'>[],
  editingId?: number,
): RootOverlap | undefined {
  const root = cleanRoot(folder);
  if (!root) return undefined;
  const sep = separatorOf(root);
  const under = (a: string, b: string) => a.startsWith(b.endsWith(sep) ? b : b + sep);
  for (const l of libraries) {
    if (l.id === editingId || l.kind !== 'books') continue;
    const other = cleanRoot(l.root);
    if (other === root) return { name: l.name, how: 'same' };
    if (under(root, other)) return { name: l.name, how: 'inside' };
    if (under(other, root)) return { name: l.name, how: 'holds' };
  }
  return undefined;
}
