import type { FsEntry, FsListing } from '@/api/types';

// Library > Folders, the logic: shaping lazily loaded listings into the rows of
// one accessible tree, which folders to open for a deep link, a folder's audio
// files, and the tree's keyboard model. Paths are library-relative and
// slash-separated ("" = the library root).

/** The folder a path sits in ("A/B" → "A", "A" → ""). */
export function parentOf(path: string): string {
  const i = path.lastIndexOf('/');
  return i < 0 ? '' : path.slice(0, i);
}

/** The folders to open so `path` shows in the tree, outermost first ("A/B/C" → ["A", "A/B"]). */
export function ancestorsOf(path: string): string[] {
  const out: string[] = [];
  for (let p = parentOf(path); p; p = parentOf(p)) out.unshift(p);
  return out;
}

/** `expanded` plus the ancestors of `path` (the same set when nothing changes). */
export function withAncestors(expanded: ReadonlySet<string>, path: string): ReadonlySet<string> {
  const missing = ancestorsOf(path).filter((p) => !expanded.has(p));
  return missing.length ? new Set([...expanded, ...missing]) : expanded;
}

/** The listings the tree shows: the root and every open folder whose ancestors are open too. */
export function visibleListings(expanded: ReadonlySet<string>): string[] {
  return ['', ...[...expanded].filter((p) => ancestorsOf(p).every((a) => expanded.has(a)))];
}

/** A listing as the tree sees it: loaded, failed, or (neither) still loading. */
export interface ListingState {
  data?: FsListing;
  failed?: boolean;
}

export type TreeRow =
  | {
      kind: 'folder';
      entry: FsEntry;
      /** aria-level: the root's folders are level 1. */
      level: number;
      posInSet: number;
      setSize: number;
      expanded: boolean;
      /** False once its listing is known to hold no folders. */
      expandable: boolean;
    }
  /** A placeholder under `parent` while its listing loads, fails, or was cut short. */
  | { kind: 'loading' | 'error' | 'more'; parent: string; level: number };

/** A listing's folders (audio files are not tree rows). */
export const foldersIn = (listing: FsListing) => listing.entries.filter((e) => e.is_dir);

/**
 * The visible rows of the tree, depth first, from the listings loaded so far.
 * An open folder whose listing hasn't arrived shows a loading row (or an error
 * row) in place of its children.
 */
export function treeRows(
  listing: (path: string) => ListingState | undefined,
  expanded: ReadonlySet<string>,
): TreeRow[] {
  const rows: TreeRow[] = [];
  const walk = (parent: string, level: number) => {
    const state = listing(parent);
    if (!state?.data) {
      rows.push({ kind: state?.failed ? 'error' : 'loading', parent, level });
      return;
    }
    const folders = foldersIn(state.data);
    folders.forEach((entry, i) => {
      const own = listing(entry.path)?.data;
      const open = expanded.has(entry.path);
      const expandable = own ? foldersIn(own).length > 0 : true;
      rows.push({
        kind: 'folder',
        entry,
        level,
        posInSet: i + 1,
        setSize: folders.length,
        expanded: open && expandable,
        expandable,
      });
      if (open && expandable) walk(entry.path, level + 1);
    });
    if (state.data.next_offset) rows.push({ kind: 'more', parent, level });
  };
  walk('', 1);
  return rows;
}

/** What a key press on the focused row does (WAI-ARIA tree pattern), or null for nothing. */
export type TreeKeyAction =
  | { type: 'focus'; path: string }
  | { type: 'expand'; path: string }
  | { type: 'collapse'; path: string }
  | { type: 'select'; path: string }
  | null;

export function treeKeyAction(rows: TreeRow[], focused: string, key: string): TreeKeyAction {
  const folders = rows.filter((r) => r.kind === 'folder');
  const i = folders.findIndex((r) => r.entry.path === focused);
  if (i < 0) return null;
  const row = folders[i];
  const focus = (r: (typeof folders)[number] | undefined): TreeKeyAction =>
    r ? { type: 'focus', path: r.entry.path } : null;
  switch (key) {
    case 'ArrowDown':
      return focus(folders[i + 1]);
    case 'ArrowUp':
      return focus(folders[i - 1]);
    case 'Home':
      return focus(folders[0]);
    case 'End':
      return focus(folders[folders.length - 1]);
    case 'ArrowRight':
      if (!row.expandable) return null;
      if (!row.expanded) return { type: 'expand', path: row.entry.path };
      // Its first child, if it has loaded.
      return folders[i + 1]?.level === row.level + 1 ? focus(folders[i + 1]) : null;
    case 'ArrowLeft': {
      if (row.expanded) return { type: 'collapse', path: row.entry.path };
      const parent = parentOf(row.entry.path);
      return parent ? { type: 'focus', path: parent } : null;
    }
    case 'Enter':
    case ' ':
      return { type: 'select', path: row.entry.path };
    default:
      return null;
  }
}

/** A folder's own entry from its parent's listing (its override, whether it is a book). */
export function entryIn(parent: FsListing | undefined, path: string): FsEntry | undefined {
  return parent?.entries.find((e) => e.is_dir && e.path === path);
}

/** The audio files directly in a folder, with their total size and (when every file knows it) length. */
export function audioFilesOf(listing: FsListing) {
  const files = listing.entries.filter((e) => !e.is_dir && e.is_audio);
  const size = files.reduce((n, f) => n + f.size, 0);
  const duration = files.every((f) => f.duration)
    ? files.reduce((n, f) => n + (f.duration ?? 0), 0)
    : undefined;
  return { files, size, duration };
}

/** A folder's full path on the server: the library root, then the library-relative path. */
export function fullPath(root: string, path: string): string {
  const base = root.replace(/[\\/]+$/, '');
  return path ? `${base}/${path}` : root;
}

/** The last segment of a library-relative path. */
export const baseName = (path: string) => path.slice(path.lastIndexOf('/') + 1);

const BYTE_UNITS = ['kilobyte', 'megabyte', 'gigabyte', 'terabyte'] as const;

/** 1,310,000,000 → "1.3 GB": decimal units, from kB up (an audio file is never a few bytes). */
export function formatBytes(bytes: number, lang: string): string {
  let n = Math.max(0, bytes) / 1000;
  let u = 0;
  while (n >= 1000 && u < BYTE_UNITS.length - 1) {
    n /= 1000;
    u++;
  }
  return new Intl.NumberFormat(lang, {
    style: 'unit',
    unit: BYTE_UNITS[u],
    unitDisplay: 'short',
    maximumFractionDigits: n >= 100 ? 0 : 1,
  }).format(n);
}

/** 4,530 s → "1h 15m"; under an hour, minutes; under a minute, seconds. */
export function formatDuration(seconds: number, lang: string): string {
  const unit = (n: number, u: 'hour' | 'minute' | 'second') =>
    new Intl.NumberFormat(lang, { style: 'unit', unit: u, unitDisplay: 'narrow' }).format(n);
  const s = Math.round(Math.max(0, seconds));
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  if (h) return m ? `${unit(h, 'hour')} ${unit(m, 'minute')}` : unit(h, 'hour');
  return m ? unit(m, 'minute') : unit(s, 'second');
}
