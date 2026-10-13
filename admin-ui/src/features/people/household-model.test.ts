import type { AdminUser } from '@/api/types';
import { sam } from '@/test/fixtures';
import {
  assignBlocked,
  assignPatch,
  assignTargets,
  defaultAccess,
  displayName,
  peopleGroups,
  type PeopleEntry,
  independentPatch,
  keptMembers,
  ownerName,
} from './household-model';

function person(id: number, username: string, over: Partial<AdminUser> = {}): AdminUser {
  return { ...sam, id, username, ...over };
}

/** A member of `owner`. */
function memberOf(
  owner: AdminUser,
  id: number,
  username: string,
  kind: 'kid' | 'grownup' = 'kid',
): AdminUser {
  return person(id, username, {
    managed_by: { id: owner.id, username: owner.username },
    member_kind: kind,
    member_access: kind === 'kid' ? 'collections' : 'all',
    can_add_members: false,
  });
}

const adam = person(1, 'adam', { members_count: 2 });
const bea = person(2, 'bea');
const evie = memberOf(adam, 3, 'evie');
const ellis = memberOf(adam, 4, 'ellis', 'grownup');
const ids = (list: AdminUser[]) => list.map((u) => u.username);

/** An entry as text: a person's username, or "owner[member,member]" for a household. */
const shape = (entries: PeopleEntry[]) =>
  entries.map((e) =>
    e.kind === 'person' ? e.user.username : `${e.owner.username}[${ids(e.members).join(',')}]`,
  );

describe('peopleGroups', () => {
  it('gathers each household into one entry where the owner stands, keeping the server order', () => {
    // The server lists by name: ellis and evie come before their owner.
    expect(shape(peopleGroups([adam, bea, ellis, evie]))).toEqual(['adam[ellis,evie]', 'bea']);
    expect(shape(peopleGroups([ellis, bea, evie, adam]))).toEqual(['bea', 'adam[ellis,evie]']);
  });

  it('leaves ordinary people as people', () => {
    const cy = person(5, 'cy');
    expect(shape(peopleGroups([bea, cy]))).toEqual(['bea', 'cy']);
  });

  it("keeps a member whose owner isn't in the list as a person, in its own place", () => {
    expect(shape(peopleGroups([bea, evie]))).toEqual(['bea', 'evie']);
    expect(shape(peopleGroups([evie, bea]))).toEqual(['evie', 'bea']);
  });

  it('shows an owner with none of their members listed as a person', () => {
    expect(peopleGroups([adam, bea])).toEqual([
      { kind: 'person', user: adam },
      { kind: 'person', user: bea },
    ]);
  });

  it('groups one level only: a member of a member keeps its own place', () => {
    const odd = memberOf(evie, 9, 'odd');
    expect(shape(peopleGroups([odd, adam, evie]))).toEqual(['odd', 'adam[evie]']);
  });

  it('shows everyone exactly once', () => {
    const all = [adam, bea, evie, ellis];
    const shown = peopleGroups(all).flatMap((e) =>
      e.kind === 'person' ? [e.user] : [e.owner, ...e.members],
    );
    expect(shown).toHaveLength(all.length);
    expect(new Set(shown)).toEqual(new Set(all));
  });
});

describe('names', () => {
  it('prefers the display name, falling back to the username', () => {
    expect(displayName({ username: 'evie', display_name: 'Evie' })).toBe('Evie');
    expect(displayName({ username: 'evie', display_name: '  ' })).toBe('evie');
  });

  it("names a member's owner by display name when known, else by the record's username", () => {
    const named = { ...adam, display_name: 'Adam' };
    expect(ownerName(evie, [named, evie])).toBe('Adam');
    expect(ownerName(evie, [evie])).toBe('adam');
    expect(ownerName(bea, [adam, bea])).toBeUndefined();
  });
});

describe('assigning', () => {
  it('offers anyone else who is neither a member nor a demo account, minus the current owner', () => {
    const demo = person(5, 'guest', { is_demo: true });
    const admin = person(6, 'chris', { role: 'admin' });
    const everyone = [adam, bea, evie, ellis, demo, admin];
    expect(ids(assignTargets(everyone, bea))).toEqual(['adam', 'chris']);
    // A member moves to someone new: their own owner isn't offered.
    expect(ids(assignTargets(everyone, evie))).toEqual(['bea', 'chris']);
  });

  it('refuses admins, people with members of their own, and demo accounts', () => {
    expect(assignBlocked(person(6, 'chris', { role: 'admin' }))).toBe('admin');
    expect(assignBlocked(adam)).toBe('hasMembers');
    expect(assignBlocked(person(5, 'guest', { is_demo: true }))).toBe('demo');
    expect(assignBlocked(bea)).toBeUndefined();
    expect(assignBlocked(evie)).toBeUndefined();
  });

  it('starts a kid on shared collections and a grown-up on everything', () => {
    expect(defaultAccess('kid')).toBe('collections');
    expect(defaultAccess('grownup')).toBe('all');
  });

  it('sends the owner with the kind and access, and null to make someone independent', () => {
    expect(assignPatch(1, 'kid', 'collections')).toEqual({
      managed_by: 1,
      member_kind: 'kid',
      member_access: 'collections',
    });
    expect(independentPatch).toEqual({ managed_by: null });
  });
});

describe('keptMembers', () => {
  const members = [{ id: 3 }, { id: 4 }, { id: 7 }];

  it('keeps every member not ticked to be deleted, in list order', () => {
    expect(keptMembers(members, new Set([3, 4, 7]))).toEqual([]);
    expect(keptMembers(members, new Set([4]))).toEqual([3, 7]);
    expect(keptMembers(members, new Set())).toEqual([3, 4, 7]);
  });

  it('ignores ticks for ids that are not members', () => {
    expect(keptMembers(members, new Set([3, 4, 7, 99]))).toEqual([]);
    expect(keptMembers([], new Set([1]))).toEqual([]);
  });
});
