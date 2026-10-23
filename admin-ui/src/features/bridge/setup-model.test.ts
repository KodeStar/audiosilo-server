import { describe, expect, it } from 'vitest';
import type { BridgeLibrary, BridgeSetup, BridgeSetupItem, BridgeStatus } from '@/api/types';
import en from '@/i18n/locales/en.json';
import {
  agentCredentialSet,
  billingKnown,
  generateNeed,
  githubStep,
  KNOWN_SETUP_ITEMS,
  setupComplete,
  setupItemLabel,
  setupItemLink,
  setupProgress,
  setupState,
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
    expect(setupItemLink(ui, 'agent')).toBe('http://192.168.1.20:8090/#/accounts');
    expect(setupItemLink(ui, 'github')).toBe('http://192.168.1.20:8090/#/accounts');
    expect(setupItemLink(ui, 'libraries')).toBe('http://192.168.1.20:8090/#/libraries');
    expect(setupItemLink(ui, 'budget')).toBe('http://192.168.1.20:8090/#/settings');
    expect(setupItemLink(ui, 'paired')).toBeUndefined();
    expect(setupItemLink(ui, 'audible_login')).toBeUndefined();
    // An id the Bridge may send that names an Object.prototype member links nowhere.
    expect(setupItemLink(ui, 'constructor')).toBeUndefined();
    expect(setupItemLink(undefined, 'agent')).toBeUndefined();
    expect(bridgeTabLink('javascript:alert(1)', 'accounts')).toBeUndefined();
  });
});

describe('what generating waits on', () => {
  it("names the first required step it needs, in the checklist's order", () => {
    const s = status({
      setup: setup([item('paired', true), item('asr', false), item('agent', false)]),
    });
    expect(generateNeed(s, 1)).toBe('asr');
    expect(
      generateNeed(status({ setup: setup([item('asr', true), item('agent', false)]) }), 1),
    ).toBe('agent');
  });

  it("counts the libraries step only when this library is the one it can't write to", () => {
    const s = status({ setup: setup([item('libraries', false), item('agent', true)]) });
    expect(generateNeed(s, 1)).toBeUndefined();
    expect(generateNeed(s, 2)).toBe('libraries');
    expect(generateNeed(s, 9)).toBe('libraries');
  });

  it('never waits on the budget, GitHub, or a step it knows nothing of', () => {
    const s = status({
      setup: setup([item('budget', false), item('github', false, false), item('audible', false)]),
    });
    expect(generateNeed(s, 1)).toBeUndefined();
  });

  it('says nothing without an online Bridge and its checklist', () => {
    const s = setup([item('agent', false)]);
    expect(generateNeed(status({ online: false, setup: s }), 1)).toBeUndefined();
    expect(generateNeed(status(), 1)).toBeUndefined();
    expect(generateNeed(undefined, 1)).toBeUndefined();
    // An optional step not done is never a need.
    expect(
      generateNeed(status({ setup: setup([item('agent', false, false)]) }), 1),
    ).toBeUndefined();
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
