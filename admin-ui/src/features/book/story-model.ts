import type { MetaWork } from '@/api/types';

// The book page's Characters and recaps card (BRIDGE-PLAN.md Phase 1e): where
// each dimension came from, and the rows it lists. Pure: no React, no i18n.

/** The card's anchor: the Community card's "covered" line links here. */
export const STORY_CARD_ID = 'characters-and-recaps';

/** Rows shown per tab before "Show all". */
export const STORY_FIRST_ROWS = 10;

/**
 * Where a dimension's entries came from: the community, this server's
 * generated files (the Bridge), or nowhere (none yet). /meta fills a dimension
 * from generated files only when the community has none of it, so a dimension
 * has one source.
 */
export type StorySource = 'community' | 'generated' | 'none';

export interface StoryDimension {
  source: StorySource;
  /** When it was generated (RFC3339), for a generated dimension. */
  generatedAt?: string;
  /**
   * The community issue a generate job filed for it (an http(s) link), shown as
   * "Contributed, waiting for review" while the dimension is still generated
   * here: once the community has it, the community's entries are what shows.
   */
  contributed?: string;
}

export interface StorySources {
  characters: StoryDimension;
  recaps: StoryDimension;
}

/**
 * Each dimension's source, from the /meta work (undefined without an answer)
 * and the newest finished generate job's community issues, by what each offers.
 */
export function storySources(
  work: MetaWork | undefined,
  contributed: Partial<Record<string, string>> = {},
): StorySources {
  const dimension = (has: boolean, generated: boolean, what: string): StoryDimension => {
    if (!has) return { source: 'none' };
    if (!generated) return { source: 'community' };
    const out: StoryDimension = { source: 'generated' };
    if (work?.generated?.generated_at) out.generatedAt = work.generated.generated_at;
    const url = contributed[what];
    if (url) out.contributed = url;
    return out;
  };
  return {
    characters: dimension(
      Boolean(work?.characters?.length),
      Boolean(work?.generated?.characters),
      'characters',
    ),
    recaps: dimension(
      Boolean(work?.recaps?.length || hasSummary(work)),
      Boolean(work?.generated?.recaps),
      'recaps',
    ),
  };
}

/** Whether the work has a whole-book summary with any text. */
export function hasSummary(work: MetaWork | undefined): boolean {
  const s = work?.recap_summary;
  return Boolean(s?.in_short?.trim() || s?.ending?.trim());
}

export type StoryRecap = NonNullable<MetaWork['recaps']>[number];

/**
 * The recaps in chapter order (stable within a chapter): "previously, in
 * earlier books" (chapter 0) first.
 */
export function recapRows(work: MetaWork | undefined): StoryRecap[] {
  return [...(work?.recaps ?? [])].sort((a, b) => a.through.chapter - b.through.chapter);
}

/** The character roles audiosilo-meta's schema allows, each with a label of its own. */
export const CHARACTER_ROLES = ['protagonist', 'antagonist', 'supporting', 'minor'] as const;

/** A role's label key, or undefined for one the console has no label for (shown as sent). */
export function roleKey(role: string | undefined): string | undefined {
  return role && (CHARACTER_ROLES as readonly string[]).includes(role)
    ? `book.story.role.${role}`
    : undefined;
}
