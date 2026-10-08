import {
  AlertCircle,
  Combine,
  Copy,
  Disc3,
  Globe,
  ImageOff,
  ImageUp,
  List,
  ListTree,
  RefreshCw,
  Repeat,
  Sparkles,
  Split,
  type LucideIcon,
} from 'lucide-react';
import {
  FITTED_STATUSES,
  type AdminBook,
  type IssueCount,
  type IssueKind,
  type IssuesSummary,
} from '@/api/types';
import type { Phrase } from '@/lib/phrase';
import { relBaseName, relParent } from '@/lib/paths';

// The Health page's categories (STYLEGUIDE.md "Health triage"): how each looks,
// what a row says about its book, and the one fix it offers. Kept out of the
// components so the wording rules are testable.

/** A category's icon and tone (status colours only, each with its label). */
export const CATEGORY_LOOK: Record<IssueKind, { icon: LucideIcon; tile: string }> = {
  scan_error: { icon: AlertCircle, tile: 'bg-destructive-soft text-destructive' },
  suspect: { icon: Split, tile: 'bg-brand-soft text-brand-ink' },
  split_discs: { icon: Disc3, tile: 'bg-brand-soft text-brand-ink' },
  duplicate: { icon: Copy, tile: 'bg-info-soft text-info' },
  no_cover: { icon: ImageOff, tile: 'bg-warning-soft text-warning' },
  unmatched: { icon: Globe, tile: 'bg-prov-community-soft text-prov-community' },
  no_chapters: { icon: List, tile: 'bg-muted text-muted-foreground' },
  detailed_chapters: { icon: ListTree, tile: 'bg-prov-community-soft text-prov-community' },
  transcode: { icon: Repeat, tile: 'bg-warning-soft text-warning' },
};

/**
 * What fixing a book under a category does: open its page (a cover upload, or the
 * match dialog), open its folder's detection, read its files again, join its
 * folder's disc folders into one book, use the community's detailed chapters, or
 * nothing the console can do (the row only offers Ignore).
 */
export type IssueFix = 'cover' | 'match' | 'folder' | 'rescan' | 'join' | 'chapters' | null;

export const FIXES: Record<IssueKind, IssueFix> = {
  scan_error: 'rescan',
  suspect: 'folder',
  split_discs: 'join',
  duplicate: null, // the compare view has its own actions
  no_cover: 'cover',
  unmatched: 'match',
  no_chapters: null,
  detailed_chapters: 'chapters',
  transcode: null,
};

/** Each fix's icon and label key. */
export const FIX_LOOK: Record<NonNullable<IssueFix>, { icon: LucideIcon; label: string }> = {
  cover: { icon: ImageUp, label: 'health.fix.cover' },
  match: { icon: Sparkles, label: 'health.fix.match' },
  folder: { icon: Split, label: 'health.fix.folder' },
  rescan: { icon: RefreshCw, label: 'health.fix.rescan' },
  join: { icon: Combine, label: 'health.fix.join' },
  chapters: { icon: ListTree, label: 'health.fix.chapters' },
};

export { say, type Phrase } from '@/lib/phrase';

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
    case 'split_discs':
      // Listed by its first disc: the book is the folder holding the discs.
      return {
        key: 'health.reason.split_discs',
        values: { folder: relBaseName(relParent(b.path)) },
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
    case 'detailed_chapters':
      return { key: 'health.reason.detailed_chapters', values: { count: b.chapter_count } };
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

/**
 * Why a book without chapters didn't get the community's (the i18n key), or
 * undefined: not checked, or a check that fitted.
 */
export function chaptersCheckNote(kind: IssueKind, b: AdminBook): string | undefined {
  if (kind !== 'no_chapters' || !b.chapters_check || FITTED_STATUSES.includes(b.chapters_check)) {
    return undefined;
  }
  return `health.chaptersCheck.${b.chapters_check}`;
}

export function isBookKind(kind: IssueKind): kind is BookIssueKind {
  return kind !== 'duplicate';
}
