import {
  AlertCircle,
  Copy,
  Globe,
  ImageOff,
  ImageUp,
  List,
  RefreshCw,
  Repeat,
  Sparkles,
  Split,
  type LucideIcon,
} from 'lucide-react';
import type { TFunction } from 'i18next';
import type { AdminBook, IssueCount, IssueKind, IssuesSummary } from '@/api/types';
import { formatNumber } from '@/lib/format';
import { relBaseName } from '@/lib/paths';

// The Health page's categories (STYLEGUIDE.md "Health triage"): how each looks,
// what a row says about its book, and the one fix it offers. Kept out of the
// components so the wording rules are testable.

/** A category's icon and tone (status colours only, each with its label). */
export const CATEGORY_LOOK: Record<IssueKind, { icon: LucideIcon; tile: string }> = {
  scan_error: { icon: AlertCircle, tile: 'bg-destructive-soft text-destructive' },
  suspect: { icon: Split, tile: 'bg-brand-soft text-brand-ink' },
  duplicate: { icon: Copy, tile: 'bg-info-soft text-info' },
  no_cover: { icon: ImageOff, tile: 'bg-warning-soft text-warning' },
  unmatched: { icon: Globe, tile: 'bg-prov-community-soft text-prov-community' },
  no_chapters: { icon: List, tile: 'bg-muted text-muted-foreground' },
  transcode: { icon: Repeat, tile: 'bg-warning-soft text-warning' },
};

/**
 * What fixing a book under a category does: open its page (a cover upload, or the
 * match dialog), open its folder's detection, read its files again, or nothing
 * the console can do (the row only offers Ignore).
 */
export type IssueFix = 'cover' | 'match' | 'folder' | 'rescan' | null;

export const FIXES: Record<IssueKind, IssueFix> = {
  scan_error: 'rescan',
  suspect: 'folder',
  duplicate: null, // the compare view has its own actions
  no_cover: 'cover',
  unmatched: 'match',
  no_chapters: null,
  transcode: null,
};

/** Each fix's icon and label key. */
export const FIX_LOOK: Record<NonNullable<IssueFix>, { icon: LucideIcon; label: string }> = {
  cover: { icon: ImageUp, label: 'health.fix.cover' },
  match: { icon: Sparkles, label: 'health.fix.match' },
  folder: { icon: Split, label: 'health.fix.folder' },
  rescan: { icon: RefreshCw, label: 'health.fix.rescan' },
};

/** An i18n key and its values: a row's reason, worded where it's shown. */
export interface Phrase {
  key: string;
  values?: Record<string, string | number>;
}

/**
 * A phrase in words: its values plus `formatted`, the count with the language's
 * separators (the count itself picks the plural form).
 */
export function say(t: TFunction, p: Phrase, lang: string): string {
  const count = p.values?.count;
  return t(
    p.key,
    typeof count === 'number' ? { ...p.values, formatted: formatNumber(count, lang) } : p.values,
  );
}

/** Hours, rounded down, for "one chapter over N hours". */
const hours = (seconds: number) => Math.floor(seconds / 3600);

/** Why a book is listed under a category. */
export function issueReason(kind: IssueKind, b: AdminBook): Phrase {
  switch (kind) {
    case 'scan_error': {
      const file = b.scan_error_file ? relBaseName(b.scan_error_file) : '';
      const code = b.scan_error ?? 'unreadable';
      return { key: `health.reason.${code}`, values: { file, detail: b.scan_error_detail ?? '' } };
    }
    case 'suspect':
      return {
        key: 'health.reason.suspect',
        values: { count: b.suspect_parts ?? 2, files: b.file_count },
      };
    case 'no_cover':
      return { key: 'health.reason.no_cover' };
    case 'unmatched':
      return { key: 'health.reason.unmatched' };
    case 'no_chapters':
      return {
        key: b.chapter_count ? 'health.reason.one_chapter' : 'health.reason.no_chapters',
        values: { hours: hours(b.duration) },
      };
    case 'transcode':
      return { key: 'health.reason.transcode', values: { codec: (b.codec || '?').toUpperCase() } };
    case 'duplicate':
      return { key: 'health.reason.duplicate' };
  }
}

/**
 * The category to show first: the URL's, else the first that needs attention,
 * else the first listed. Undefined while there are none.
 */
export function pickCategory(
  categories: readonly IssueCount[],
  wanted: string | undefined,
): IssueCount | undefined {
  return (
    categories.find((c) => c.kind === wanted) ??
    categories.find((c) => c.count > 0) ??
    categories[0]
  );
}

/** How many things need attention: every category's count plus the offline libraries. */
export function attentionTotal(summary: IssuesSummary): number {
  return summary.categories.reduce((n, c) => n + c.count, 0) + summary.offline.length;
}

/** The kinds that list books (all but duplicates, which come as groups). */
export type BookIssueKind = Exclude<IssueKind, 'duplicate'>;

export function isBookKind(kind: IssueKind): kind is BookIssueKind {
  return kind !== 'duplicate';
}
