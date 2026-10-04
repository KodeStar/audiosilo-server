// Path helpers for the console's pickers. Content is addressed by its
// library-relative path (slash-separated, "" = the library root); library roots
// are absolute paths in the server's own syntax.

export interface Crumb {
  label: string;
  path: string;
}

/** Breadcrumbs for a library-relative path ("" = the library root). */
export function libraryCrumbs(libraryName: string, path: string): Crumb[] {
  const out: Crumb[] = [{ label: libraryName, path: '' }];
  let acc = '';
  for (const seg of path.split('/').filter(Boolean)) {
    acc = acc ? `${acc}/${seg}` : seg;
    out.push({ label: seg, path: acc });
  }
  return out;
}

/** Whether a path names a folder from the filesystem root: "/mnt/x" or "C:\\x". */
export function isAbsolutePath(p: string): boolean {
  return p.startsWith('/') || /^[A-Za-z]:[\\/]/.test(p);
}

/** The separator a server path uses: "\\" for a Windows path, "/" otherwise. */
const separatorOf = (path: string) => (/^[A-Za-z]:/.test(path) ? '\\' : '/');

/** The last segment of a library-relative path ("A/B" → "B"). */
export const relBaseName = (path: string) => path.slice(path.lastIndexOf('/') + 1);

/** The folder a library-relative path sits in ("A/B" → "A", "A" → ""). */
export function relParent(path: string): string {
  const i = path.lastIndexOf('/');
  return i < 0 ? '' : path.slice(0, i);
}

/**
 * A library-relative path as a full path on the server, in the root's own
 * syntax ("C:\\Books" + "A/B" → "C:\\Books\\A\\B"). An empty `rel` is the root
 * itself; without a root (libraries still loading) it is `rel` as is.
 */
export function joinLibraryPath(root: string | undefined, rel: string): string {
  if (!root) return rel;
  if (!rel) return root;
  const sep = separatorOf(root);
  const base = root.replace(/[\\/]+$/, '');
  return `${base}${sep}${sep === '/' ? rel : rel.replaceAll('/', sep)}`;
}

/** The last folder name of an absolute server path ("" for a root). */
export function absoluteBaseName(path: string): string {
  return path.split(separatorOf(path)).filter(Boolean).pop() ?? '';
}

/**
 * Breadcrumbs for an absolute server path, in the server's own syntax: "/" and
 * "/mnt/books", or "C:\\" and "C:\\Books" on Windows (where the first part is
 * the drive, whose root is "C:\\").
 */
export function absoluteCrumbs(path: string): Crumb[] {
  const sep = separatorOf(path);
  const parts = path.split(sep).filter(Boolean);
  const windows = sep === '\\';
  if (windows && parts.length === 0) return [{ label: path, path }];
  const root = windows ? `${parts.shift()}\\` : '/';
  const out: Crumb[] = [{ label: root, path: root }];
  let acc = windows ? root.slice(0, -1) : '';
  for (const p of parts) {
    acc += sep + p;
    out.push({ label: p, path: acc });
  }
  return out;
}
