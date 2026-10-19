import type { BridgeSetup, BridgeSetupItem, BridgeStatus } from '@/api/types';
import { mappingState, safeHttpUrl } from './bridge-model';
import { titleCase } from './generate-model';

// The Bridge's setup checklist (BRIDGE-PLAN.md Phase 2b, FIRST-RUN-PLAN.md F2)
// as the console shows it: Settings > Bridge's Setup card, the first-run flow's
// "Add a Bridge" step (both through BridgeSetupList), and the book page's line
// saying which step Generate waits on. The Bridge sends the checklist in its
// heartbeat; sign-in and the rest happen on the Bridge's own page (the server
// never calls the Bridge). Pure: no React, no i18n instance.

/** The steps this console has words for, in the Bridge's order. */
export const KNOWN_SETUP_ITEMS = [
  'paired',
  'libraries',
  'asr',
  'agent',
  'budget',
  'github',
] as const;

/** A step's label key and its fallback (a step the console doesn't know reads title-cased). */
export function setupItemLabel(id: string): { key: string; fallback: string } {
  return { key: `bridge.setup.item.${id}`, fallback: titleCase(id) };
}

/** Whether a step's id is one this console words (else it shows title-cased, with no hint). */
export function knownSetupItem(id: string): boolean {
  return (KNOWN_SETUP_ITEMS as readonly string[]).includes(id);
}

/**
 * The Bridge page's tab that fixes each step (its hash routes, lib/tabs.ts):
 * sign-ins on Accounts, the ASR choice and the budget in Settings. Paired has
 * nothing to fix there; an unknown step links nowhere.
 */
const SETUP_TAB: Record<string, string> = {
  libraries: 'libraries',
  asr: 'settings',
  agent: 'accounts',
  budget: 'settings',
  github: 'accounts',
};

/** The Accounts tab of the Bridge's page, where GitHub and the agent credential are signed in. */
export const ACCOUNTS_TAB = 'accounts';

/** A tab of the Bridge's page (`<ui_url>#/<tab>`), only for an http(s) ui_url. */
export function bridgeTabLink(uiUrl: string | undefined, tab: string): string | undefined {
  const base = safeHttpUrl(uiUrl);
  if (!base) return undefined;
  const u = new URL(base);
  u.hash = `/${tab}`;
  return u.href;
}

/** Where a step is fixed on the Bridge's page, when it has a tab and the page an address. */
export function setupItemLink(uiUrl: string | undefined, id: string): string | undefined {
  const tab = SETUP_TAB[id];
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

/**
 * Whether the Bridge can file contributions, as the server decides it
 * (bridge.Status.CanContribute): the checklist's answer when the Bridge sends
 * one, else its older estimates' figure, else no.
 */
export function bridgeCanContribute(
  status: Pick<BridgeStatus, 'setup' | 'estimates'> | undefined,
): boolean {
  if (status?.setup) return status.setup.can_contribute;
  return status?.estimates?.sidecars?.can_contribute ?? false;
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
