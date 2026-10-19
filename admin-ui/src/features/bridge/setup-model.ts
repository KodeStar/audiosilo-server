import type { BridgeSetup, BridgeSetupItem, BridgeStatus } from '@/api/types';
import { bridgeTabLink, mappingState } from './bridge-model';
import { titleCase } from './generate-model';

// The Bridge's setup checklist (BRIDGE-PLAN.md Phase 2b, FIRST-RUN-PLAN.md F2)
// as the console shows it: Settings > Bridge's Setup card, the first-run flow's
// "Add a Bridge" step (both through BridgeSetupList), and the book page's line
// saying which step Generate waits on. The Bridge sends the checklist in its
// heartbeat; sign-in and the rest happen on the Bridge's own page (the server
// never calls the Bridge). Pure: no React, no i18n instance.

/** The Accounts tab of the Bridge's page, where GitHub and the agent credential are signed in. */
export const ACCOUNTS_TAB = 'accounts';

/**
 * The steps this console has words for, in the Bridge's order, and the Bridge
 * page's tab that fixes each (its hash routes, lib/tabs.ts): sign-ins on
 * Accounts, the ASR choice and the budget in Settings. Paired has nothing to
 * fix there; an unknown step links nowhere. A Map, so an id like `constructor`
 * finds nothing.
 */
const SETUP_TAB = new Map<string, string | undefined>([
  ['paired', undefined],
  ['libraries', 'libraries'],
  ['asr', 'settings'],
  ['agent', ACCOUNTS_TAB],
  ['budget', 'settings'],
  ['github', ACCOUNTS_TAB],
]);

/** The steps this console has words for, in the Bridge's order. */
export const KNOWN_SETUP_ITEMS: readonly string[] = [...SETUP_TAB.keys()];

/** A step's label key and its fallback (a step the console doesn't know reads title-cased). */
export function setupItemLabel(id: string): { key: string; fallback: string } {
  return { key: `bridge.setup.item.${id}`, fallback: titleCase(id) };
}

/** Whether a step's id is one this console words (else it shows title-cased, with no hint). */
export function knownSetupItem(id: string): boolean {
  return SETUP_TAB.has(id);
}

/** Where a step is fixed on the Bridge's page, when it has a tab and the page an address. */
export function setupItemLink(uiUrl: string | undefined, id: string): string | undefined {
  const tab = SETUP_TAB.get(id);
  return tab ? bridgeTabLink(uiUrl, tab) : undefined;
}

/** How a step reads: done, still needed, or optional and not done. */
export type SetupState = 'done' | 'todo' | 'optional';

export function setupState(item: Pick<BridgeSetupItem, 'done' | 'required'>): SetupState {
  if (item.done) return 'done';
  return item.required ? 'todo' : 'optional';
}

/** The required steps, and how many are done. */
export function setupProgress(setup: Pick<BridgeSetup, 'items'>): { done: number; total: number } {
  const required = setup.items.filter((i) => i.required);
  return { done: required.filter((i) => i.done).length, total: required.length };
}

/** Every required step is done (the Bridge's "Get set up" card goes away then). */
export function setupComplete(setup: Pick<BridgeSetup, 'items'>): boolean {
  const { done, total } = setupProgress(setup);
  return done === total;
}

/**
 * Whether the agent credential is set, as the agent step says; undefined when
 * the checklist has no agent step (nothing to say).
 */
export function agentCredentialSet(setup: Pick<BridgeSetup, 'items'>): boolean | undefined {
  return setup.items.find((i) => i.id === 'agent')?.done;
}

/** The steps generating characters and recaps waits on: the ones it can't run without. */
export type GenerateNeed = 'asr' | 'agent' | 'libraries';

/**
 * The first required step still to do that generating for a book in this
 * library needs, in the checklist's order: speech recognition, the agent's
 * credential, or this library's mapping (the libraries step counts only when
 * this library is the one the Bridge can't write to yet). Undefined without an
 * online Bridge's checklist, or when nothing it needs is missing (budget and
 * GitHub never stop a run here: the budget is a cap, GitHub only for
 * contributing).
 */
export function generateNeed(
  status: Pick<BridgeStatus, 'online' | 'setup' | 'libraries'> | undefined,
  libraryId: number,
): GenerateNeed | undefined {
  if (!status?.online || !status.setup) return undefined;
  for (const item of status.setup.items) {
    if (!item.required || item.done) continue;
    if (item.id === 'asr' || item.id === 'agent') return item.id;
    if (item.id === 'libraries') {
      const mapping = status.libraries.find((l) => l.id === libraryId)?.mapping ?? null;
      if (mappingState(mapping) !== 'ready') return 'libraries';
    }
  }
  return undefined;
}
