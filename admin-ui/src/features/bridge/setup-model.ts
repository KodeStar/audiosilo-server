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
 * page's tab that fixes each (its hash routes, lib/tabs.ts): sign-ins (the
 * agent, GitHub, Audible) on Accounts, the ASR choice and the budget in
 * Settings. Paired has nothing to fix there; an unknown step links nowhere. A
 * Map, so an id like `constructor` finds nothing.
 */
const SETUP_TAB = new Map<string, string | undefined>([
  ['paired', undefined],
  ['libraries', 'libraries'],
  ['asr', 'settings'],
  ['agent', ACCOUNTS_TAB],
  ['budget', 'settings'],
  ['github', ACCOUNTS_TAB],
  ['audible', ACCOUNTS_TAB],
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

/** The Workers tab of the Bridge's page, where ASR workers are added. */
const WORKERS_TAB = 'workers';

/**
 * The ASR step's detail while the Bridge waits for an ASR worker (the Bridge's
 * setup.DetailASRWaiting), read only from a Bridge older than its
 * `needs_worker` flag.
 */
const ASR_WAITING_DETAIL = 'waiting for an ASR worker';

/** What stepNeedsWorker reads of a setup step. */
type WorkerProbe = Pick<BridgeSetupItem, 'id' | 'detail' | 'needs_worker'>;

/**
 * Whether a step waits for an ASR worker: its `needs_worker` flag (bridge#28),
 * else, from an older Bridge, the ASR step's detail text.
 */
function stepNeedsWorker(item: WorkerProbe): boolean {
  if (item.needs_worker) return true;
  return item.id === 'asr' && Boolean(item.detail?.includes(ASR_WAITING_DETAIL));
}

/**
 * Where a step is fixed on the Bridge's page, when it has a tab and the page an
 * address. A step waiting for an ASR worker goes to Workers (stepNeedsWorker);
 * every other ASR state to Settings.
 */
export function setupItemLink(uiUrl: string | undefined, item: WorkerProbe): string | undefined {
  const tab = stepNeedsWorker(item) ? WORKERS_TAB : SETUP_TAB.get(item.id);
  return tab ? bridgeTabLink(uiUrl, tab) : undefined;
}

/**
 * Whether an online Bridge's transcription waits for an ASR worker and none is
 * connected (BRIDGE-10): its ASR step is waiting for one (the fallback is wait,
 * or it has no speech recognition of its own), so a job that transcribes parks
 * until a worker connects rather than taking the time its figures predict.
 */
export function waitsForWorker(
  status: Pick<BridgeStatus, 'online' | 'setup' | 'workers'> | undefined,
): boolean {
  if (!status?.online || status.workers.length > 0) return false;
  const asr = status.setup?.items.find((item) => item.id === 'asr');
  return Boolean(asr && !asr.done && stepNeedsWorker(asr));
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
 * `value` when it is one of `known`'s own keys (so a later value, or one like
 * `constructor`, is none), else undefined.
 */
function knownKey<K extends string>(
  known: Readonly<Record<K, string>>,
  value: string | undefined,
): K | undefined {
  return value !== undefined && Object.hasOwn(known, value) ? (value as K) : undefined;
}

/**
 * The Setup card's words for each way the Bridge hands its agent credential to
 * the agent container (bridge#79); the one list of the modes this console knows.
 */
export const CREDENTIAL_SHARE_KEY = {
  group: 'bridge.setup.credentialShareGroup',
  world: 'bridge.setup.credentialShareWorld',
  group_required: 'bridge.setup.credentialShareGroupRequired',
} as const;

export type CredentialShare = keyof typeof CREDENTIAL_SHARE_KEY;

/**
 * The agent credential's hand-off, when the Bridge reports one this console
 * words; undefined with none (the agent container's own credential, an older
 * Bridge) or a value it doesn't know.
 */
export function credentialShare(
  setup: Pick<BridgeSetup, 'agent_credential'>,
): CredentialShare | undefined {
  return knownKey(CREDENTIAL_SHARE_KEY, setup.agent_credential);
}

/**
 * Whether the agent credential is set: a hand-off reported is one set on the
 * Bridge, even while the agent step waits on it (held back by
 * agent.require_group, say); else as the agent step says. Undefined when the
 * checklist has no agent step and no hand-off (nothing to say).
 */
export function agentCredentialSet(
  setup: Pick<BridgeSetup, 'items' | 'agent_credential'>,
): boolean | undefined {
  if (credentialShare(setup) !== undefined) return true;
  return setup.items.find((i) => i.id === 'agent')?.done;
}

/**
 * The Setup card's words for why a GitHub sign-in on the Bridge can't be used
 * now (bridge#25); the one list of the states this console knows.
 */
export const GITHUB_SIGN_IN_KEY = {
  unrefreshed: 'bridge.setup.githubUnrefreshed',
  refused: 'bridge.setup.githubRefused',
} as const;

export type GitHubSignInState = keyof typeof GITHUB_SIGN_IN_KEY;

/**
 * The GitHub sign-in's trouble, when the Bridge reports one this console
 * words; undefined otherwise (it works, there is none, or an older Bridge).
 */
export function githubSignInState(
  setup: Pick<BridgeSetup, 'github_state'>,
): GitHubSignInState | undefined {
  return knownKey(GITHUB_SIGN_IN_KEY, setup.github_state);
}

/** The checklist's GitHub step, when it has one. */
export function githubStep(setup: Pick<BridgeSetup, 'items'>): BridgeSetupItem | undefined {
  return setup.items.find((i) => i.id === 'github');
}

/**
 * Whether "Paid by" means anything yet: there is a credential to pay with (the
 * checklist's agent step done or, from a Bridge without that step, its agent
 * ready). Before that the estimates' subscription flag is only a default.
 */
export function billingKnown(status: Pick<BridgeStatus, 'setup' | 'estimates'>): boolean {
  const set = status.setup ? agentCredentialSet(status.setup) : undefined;
  return set ?? status.estimates?.sidecars?.agent_ready ?? false;
}

/** The steps generating characters and recaps waits on: the ones it can't run without. */
export type GenerateNeed = 'asr' | 'agent' | 'libraries';

/**
 * The first step still to do that generating for a book in this library
 * needs: the agent's credential first (no worker can stand in for it, so it is
 * the reason the player gives too, BRIDGE-09), then speech recognition or this
 * library's mapping in the checklist's order (the libraries step counts only
 * when this library is the one the Bridge can't write to yet). These count
 * whether or not the checklist marks them required: a Bridge where one is optional (the agent,
 * bridge#36: it runs other jobs without one) still can't generate without it,
 * and the job would park. Undefined without an online Bridge's checklist, or
 * when nothing it needs is missing (budget and GitHub never stop a run here:
 * the budget is a cap, GitHub only for contributing).
 */
export function generateNeed(
  status: Pick<BridgeStatus, 'online' | 'setup' | 'libraries'> | undefined,
  libraryId: number,
): GenerateNeed | undefined {
  if (!status?.online || !status.setup) return undefined;
  if (status.setup.items.some((item) => item.id === 'agent' && !item.done)) return 'agent';
  for (const item of status.setup.items) {
    if (item.done) continue;
    if (item.id === 'asr') return item.id;
    if (item.id === 'libraries') {
      const mapping = status.libraries.find((l) => l.id === libraryId)?.mapping ?? null;
      if (mappingState(mapping) !== 'ready') return 'libraries';
    }
  }
  return undefined;
}
