import type { FolderMode } from '@/api/types';

// The three ways AudioSilo can read a folder (STYLEGUIDE.md "Folder detection"):
// automatically (a folder that directly holds audio is one book), or an admin's
// durable, path-keyed override. Shared by Library > Folders and the per-library
// detection dialog so both name the choices the same way.

export type FolderChoice = FolderMode | 'auto';

/** In display order. Each is named by `folders.mode.<choice>` (a radio card's title, a select's option). */
export const FOLDER_CHOICES: readonly FolderChoice[] = ['auto', 'book', 'collection'];

/** A folder's current choice from its listing entry's `override`. */
export const choiceOf = (override?: FolderMode | ''): FolderChoice => override || 'auto';

/** What to send for a choice: the override, or null to clear it (back to automatic). */
export const modeOf = (choice: FolderChoice): FolderMode | null =>
  choice === 'auto' ? null : choice;
