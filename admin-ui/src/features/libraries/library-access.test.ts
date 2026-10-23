import { describe, expect, it } from 'vitest';
import type { AdminModule, AdminUser, SetupEnvelope } from '@/api/types';
import { configLibrary, everyoneToGrant, podcastsHintKey } from './library-access';

const user = (u: Partial<AdminUser>): AdminUser =>
  ({ id: 1, username: 'u', role: 'user', disabled: false, ...u }) as AdminUser;

describe('everyoneToGrant', () => {
  it('grants every enabled listener', () => {
    const users = [user({ id: 1 }), user({ id: 2 })];
    expect(everyoneToGrant(users).map((u) => u.id)).toEqual([1, 2]);
  });
  it('leaves out admins, disabled accounts, household members and the demo account', () => {
    const users = [
      user({ id: 1, role: 'admin' }),
      user({ id: 2, disabled: true }),
      user({ id: 3, managed_by: { id: 9, username: 'o' } } as Partial<AdminUser>),
      user({ id: 4, is_demo: true }),
      user({ id: 5 }),
    ];
    expect(everyoneToGrant(users).map((u) => u.id)).toEqual([5]);
  });
});

describe('configLibrary', () => {
  const setup = { config_libraries: ['Audiobooks'] } as unknown as SetupEnvelope;
  it('names a library config.yaml declares', () => {
    expect(configLibrary(setup, 'Audiobooks')).toBe(true);
  });
  it('leaves out one the console added, and an unloaded setup', () => {
    expect(configLibrary(setup, 'Podcasts')).toBe(false);
    expect(configLibrary(undefined, 'Audiobooks')).toBe(false);
  });
});

describe('podcastsHintKey', () => {
  const mod = (m: Partial<AdminModule>): AdminModule =>
    ({ id: 'podcasts', enabled: false, active: false, bridge_state: 'ready', ...m }) as AdminModule;
  it('names what a podcast library still needs', () => {
    expect(podcastsHintKey(mod({ enabled: true }))).toBe('libraries.form.kindHintReady');
    expect(podcastsHintKey(mod({ enabled: true, bridge_state: 'offline' }))).toBe(
      'libraries.form.kindHintBridge',
    );
    expect(podcastsHintKey(mod({}))).toBe('libraries.form.kindHintModule');
    expect(podcastsHintKey(mod({ bridge_state: 'no_feature' }))).toBe('libraries.form.kindHint');
  });
  it('falls back to the general hint before the modules load', () => {
    expect(podcastsHintKey(undefined)).toBe('libraries.form.kindHint');
  });
});
