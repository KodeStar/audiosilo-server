import { describe, expect, it } from 'vitest';
import type { BridgeLibrary, BridgeSetup, BridgeSetupItem, BridgeStatus } from '@/api/types';
import en from '@/i18n/locales/en.json';
import {
  agentCredentialSet,
  billingKnown,
  CREDENTIAL_SHARE_KEY,
  credentialShare,
  generateNeed,
  GITHUB_SIGN_IN_KEY,
  githubSignInState,
  githubStep,
  KNOWN_SETUP_ITEMS,
  setupComplete,
  setupItemLabel,
  setupItemLink,
  setupProgress,
  setupState,
  waitsForWorker,
} from './setup-model';
import { bridgeTabLink } from './bridge-model';

const strings = en as Record<string, string>;

const item = (id: string, done: boolean, required = true): BridgeSetupItem => ({
  id,
  done,
  required,
});

function setup(items: BridgeSetupItem[], over: Partial<BridgeSetup> = {}): BridgeSetup {
  return { items, can_contribute: false, ...over };
}

const writable = (id: number): BridgeLibrary => ({
  id,
  name: `L${id}`,
  root: `/books/${id}`,
  mapping: {
    library_id: id,
    mount_path: `/libraries/${id}`,
    mapped: true,
    writable: true,
    checked_path: 'A/B',
    error: '',
  },
});

function status(over: Partial<BridgeStatus> = {}): BridgeStatus {
  return {
    paired: true,
    paired_by: null,
    pairing: null,
    online: true,
    kinds: [],
    features: [],
    libraries: [writable(1), { id: 2, name: 'L2', root: '/books/2', mapping: null }],
    workers: [],
    ...over,
  };
}

describe('the setup checklist', () => {
  it('words every known step, and a new one title-cased', () => {
    for (const id of KNOWN_SETUP_ITEMS) {
      expect(strings[setupItemLabel(id).key]).toBeTruthy();
      expect(strings[`bridge.setup.hint.${id}`]).toBeTruthy();
    }
    expect(setupItemLabel('audible_login')).toEqual({
      key: 'bridge.setup.item.audible_login',
      fallback: 'Audible login',
    });
  });

  it('reads a step as done, to do, or optional', () => {
    expect(setupState(item('agent', true))).toBe('done');
    expect(setupState(item('agent', false))).toBe('todo');
    expect(setupState(item('github', false, false))).toBe('optional');
    expect(setupState(item('github', true, false))).toBe('done');
  });

  it('counts only the required steps', () => {
    const s = setup([item('paired', true), item('agent', false), item('github', false, false)]);
    expect(setupProgress(s)).toEqual({ done: 1, total: 2 });
    expect(setupComplete(s)).toBe(false);
    expect(setupComplete(setup([item('paired', true), item('github', false, false)]))).toBe(true);
  });

  it('says whether the agent credential is set, or nothing without the step', () => {
    expect(agentCredentialSet(setup([item('agent', true)]))).toBe(true);
    expect(agentCredentialSet(setup([item('agent', false)]))).toBe(false);
    expect(agentCredentialSet(setup([item('paired', true)]))).toBeUndefined();
  });

  it('links a step to the Bridge page tab that fixes it, only for an http(s) page', () => {
    const ui = 'http://192.168.1.20:8090/';
    expect(setupItemLink(ui, { id: 'agent' })).toBe('http://192.168.1.20:8090/#/accounts');
    expect(setupItemLink(ui, { id: 'github' })).toBe('http://192.168.1.20:8090/#/accounts');
    expect(setupItemLink(ui, { id: 'audible' })).toBe('http://192.168.1.20:8090/#/accounts');
    expect(setupItemLink(ui, { id: 'libraries' })).toBe('http://192.168.1.20:8090/#/libraries');
    expect(setupItemLink(ui, { id: 'budget' })).toBe('http://192.168.1.20:8090/#/settings');
    // The ASR step goes to Settings, or to Workers while it waits for a worker.
    expect(setupItemLink(ui, { id: 'asr' })).toBe('http://192.168.1.20:8090/#/settings');
    expect(setupItemLink(ui, { id: 'asr', detail: 'local' })).toBe(
      'http://192.168.1.20:8090/#/settings',
    );
    expect(setupItemLink(ui, { id: 'asr', detail: 'waiting for an ASR worker' })).toBe(
      'http://192.168.1.20:8090/#/workers',
    );
    // A Bridge's needs_worker flag links Workers whatever the detail says.
    expect(setupItemLink(ui, { id: 'asr', detail: 'en attente', needs_worker: true })).toBe(
      'http://192.168.1.20:8090/#/workers',
    );
    expect(setupItemLink(ui, { id: 'asr', detail: 'local', needs_worker: false })).toBe(
      'http://192.168.1.20:8090/#/settings',
    );
    // Only the ASR step reads the detail.
    expect(setupItemLink(ui, { id: 'budget', detail: 'waiting for an ASR worker' })).toBe(
      'http://192.168.1.20:8090/#/settings',
    );
    expect(setupItemLink(ui, { id: 'paired' })).toBeUndefined();
    expect(setupItemLink(ui, { id: 'audible_login' })).toBeUndefined();
    // An id the Bridge may send that names an Object.prototype member links nowhere.
    expect(setupItemLink(ui, { id: 'constructor' })).toBeUndefined();
    expect(setupItemLink(undefined, { id: 'agent' })).toBeUndefined();
    expect(bridgeTabLink('javascript:alert(1)', 'accounts')).toBeUndefined();
  });
});

describe('a Bridge waiting for an ASR worker (BRIDGE-10)', () => {
  const waiting = { ...item('asr', false), detail: 'waiting for an ASR worker' };
  const worker = { name: 'mac', version: '1', platform: 'darwin', last_seen: '' };

  it('waits with the ASR step waiting for a worker and none connected', () => {
    expect(waitsForWorker(status({ setup: setup([waiting]) }))).toBe(true);
    // The flag, from a Bridge that sends it, whatever its detail says.
    const flagged = { ...item('asr', false), detail: 'other words', needs_worker: true };
    expect(waitsForWorker(status({ setup: setup([flagged]) }))).toBe(true);
  });

  it('does not with a worker connected, local ASR, another ASR problem, or offline', () => {
    expect(waitsForWorker(status({ setup: setup([waiting]), workers: [worker] }))).toBe(false);
    expect(
      waitsForWorker(status({ setup: setup([{ ...item('asr', true), detail: 'local' }]) })),
    ).toBe(false);
    const starting = { ...item('asr', false), detail: 'the engine is starting' };
    expect(waitsForWorker(status({ setup: setup([starting]) }))).toBe(false);
    expect(waitsForWorker(status({ online: false, setup: setup([waiting]) }))).toBe(false);
    expect(waitsForWorker(status())).toBe(false);
    expect(waitsForWorker(undefined)).toBe(false);
  });
});

describe('what generating waits on', () => {
  it('names the agent first, then the first other step it needs (BRIDGE-09)', () => {
    // No worker can stand in for the agent: it is the reason, whatever the order.
    const s = status({
      setup: setup([item('paired', true), item('asr', false), item('agent', false)]),
    });
    expect(generateNeed(s, 1)).toBe('agent');
    expect(
      generateNeed(status({ setup: setup([item('asr', true), item('agent', false)]) }), 1),
    ).toBe('agent');
    expect(
      generateNeed(
        status({
          setup: setup([item('libraries', false), item('asr', false), item('agent', true)]),
        }),
        2,
      ),
    ).toBe('libraries');
    expect(
      generateNeed(status({ setup: setup([item('asr', false), item('agent', true)]) }), 1),
    ).toBe('asr');
  });

  it("counts the libraries step only when this library is the one it can't write to", () => {
    const s = status({ setup: setup([item('libraries', false), item('agent', true)]) });
    expect(generateNeed(s, 1)).toBeUndefined();
    expect(generateNeed(s, 2)).toBe('libraries');
    expect(generateNeed(s, 9)).toBe('libraries');
  });

  it('never waits on the budget, GitHub, or a step it knows nothing of', () => {
    const s = status({
      setup: setup([
        item('budget', false),
        item('github', false, false),
        item('cloud_sync', false),
      ]),
    });
    expect(generateNeed(s, 1)).toBeUndefined();
  });

  it('says nothing without an online Bridge and its checklist', () => {
    const s = setup([item('agent', false)]);
    expect(generateNeed(status({ online: false, setup: s }), 1)).toBeUndefined();
    expect(generateNeed(status(), 1)).toBeUndefined();
    expect(generateNeed(undefined, 1)).toBeUndefined();
    // An optional step generating doesn't use is never a need.
    expect(
      generateNeed(status({ setup: setup([item('github', false, false)]) }), 1),
    ).toBeUndefined();
  });

  it('waits on the agent even where the checklist makes it optional (bridge#36)', () => {
    const optional = setup([item('paired', true), item('agent', false, false)]);
    expect(generateNeed(status({ setup: optional }), 1)).toBe('agent');
    // The checklist still counts it optional: setup is complete without it.
    expect(setupComplete(optional)).toBe(true);
    expect(generateNeed(status({ setup: setup([item('agent', true, false)]) }), 1)).toBeUndefined();
    // Speech recognition likewise, should the checklist ever make it optional.
    expect(generateNeed(status({ setup: setup([item('asr', false, false)]) }), 1)).toBe('asr');
  });
});

describe('githubStep and billingKnown', () => {
  it("finds the checklist's GitHub step", () => {
    expect(githubStep(setup([item('asr', true), item('github', true, false)]))?.done).toBe(true);
    expect(githubStep(setup([item('asr', true)]))).toBeUndefined();
  });

  it('knows how the agent is paid for only once there is a credential (BRIDGE-05)', () => {
    const est = (agent_ready: boolean) =>
      ({ sidecars: { agent_ready } }) as unknown as BridgeStatus['estimates'];
    expect(billingKnown({ setup: setup([item('agent', true)]), estimates: est(false) })).toBe(true);
    expect(billingKnown({ setup: setup([item('agent', false)]), estimates: est(true) })).toBe(
      false,
    );
    // A Bridge without the agent step: its agent being ready says so.
    expect(billingKnown({ setup: setup([item('asr', true)]), estimates: est(true) })).toBe(true);
    expect(billingKnown({ setup: undefined, estimates: est(false) })).toBe(false);
    expect(billingKnown({ setup: undefined, estimates: undefined })).toBe(false);
  });
});

describe('credentialShare and githubSignInState (bridge#79, bridge#25)', () => {
  it('words the hand-off modes it knows, and nothing for none or a later one', () => {
    expect(credentialShare({ agent_credential: 'group' })).toBe('group');
    expect(credentialShare({ agent_credential: 'world' })).toBe('world');
    expect(credentialShare({ agent_credential: 'group_required' })).toBe('group_required');
    expect(credentialShare({})).toBeUndefined();
    expect(credentialShare({ agent_credential: 'later_mode' })).toBeUndefined();
    expect(credentialShare({ agent_credential: 'constructor' })).toBeUndefined();
  });

  it("tells a sign-in's trouble apart, and nothing for none or a later state", () => {
    expect(githubSignInState({ github_state: 'unrefreshed' })).toBe('unrefreshed');
    expect(githubSignInState({ github_state: 'refused' })).toBe('refused');
    expect(githubSignInState({})).toBeUndefined();
    expect(githubSignInState({ github_state: 'later_state' })).toBeUndefined();
    expect(githubSignInState({ github_state: 'constructor' })).toBeUndefined();
  });

  it('has English words for every mode and state it knows', () => {
    for (const key of [
      ...Object.values(CREDENTIAL_SHARE_KEY),
      ...Object.values(GITHUB_SIGN_IN_KEY),
    ]) {
      expect(strings[key]).toBeTruthy();
    }
  });
});
