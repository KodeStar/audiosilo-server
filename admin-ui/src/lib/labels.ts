// A book's genres and tags (catalog/labels.go): each a list of short names,
// stored as canonical JSON (["Fantasy","Epic"], "" for none). A label is one
// label in any case, spacing aside (catalog.labelKey), so "Sci-Fi" and "sci-fi"
// are one tag; the first spelling stays. Mirrors catalog.normalizeLabels, so a
// list is refused here, with a message under the field, before a save is tried.

import { CONTROL } from '@/features/book/field-rules';

/** catalog maxLabelLen / maxLabels. */
export const MAX_LABEL = 64;
export const MAX_LABELS = 50;

/** A label trimmed, its inner spacing one space. */
export const collapse = (s: string) => s.trim().split(/\s+/).filter(Boolean).join(' ');

/** The key a label is one label by (catalog.labelKey). */
export const labelKey = (s: string) => collapse(s).toLowerCase();

/** The stored list (canonical JSON; "" or "[]" for none, as is a value that isn't a list). */
export function parseLabels(stored: string): string[] {
  if (!stored) return [];
  try {
    const v: unknown = JSON.parse(stored);
    return Array.isArray(v) ? v.filter((x): x is string => typeof x === 'string') : [];
  } catch {
    return [];
  }
}

/** The canonical value of a list, as the server stores it ("" for none). */
export const encodeLabels = (labels: string[]) => (labels.length ? JSON.stringify(labels) : '');

/** A list as one line, for a diff or a toast. */
export const labelsText = (stored: string) => parseLabels(stored).join(', ');

/**
 * Labels as an edit stores them: collapsed, blanks dropped, each once (the first
 * spelling), with the i18n key of why the server would refuse them, if it would
 * (the labels still hold the one it would refuse, so a box keeps what was typed).
 */
export function cleanLabels(labels: string[]): { labels: string[]; error?: string } {
  const out: string[] = [];
  const seen = new Set<string>();
  let error: string | undefined;
  for (const raw of labels) {
    const name = collapse(raw);
    if (!name || seen.has(labelKey(name))) continue;
    if ([...name].length > MAX_LABEL) error ??= 'book.invalid.labelTooLong';
    else if (CONTROL.test(name)) error ??= 'book.invalid.control';
    seen.add(labelKey(name));
    out.push(name);
  }
  if (out.length > MAX_LABELS) error ??= 'book.invalid.tooManyLabels';
  return error ? { labels: out, error } : { labels: out };
}

/**
 * The canonical value of an edited list (a draft is always encodeLabels' JSON),
 * with what's wrong with it.
 */
export function checkLabels(raw: string): { value: string; error?: string } {
  const v = raw.trim();
  if (!v) return { value: '' };
  let list: string[];
  try {
    const parsed: unknown = JSON.parse(v);
    if (!Array.isArray(parsed) || parsed.some((x) => typeof x !== 'string')) throw new Error();
    list = parsed as string[];
  } catch {
    return { value: v, error: 'book.invalid.labels' };
  }
  const { labels, error } = cleanLabels(list);
  return error ? { value: v, error } : { value: encodeLabels(labels) };
}
