import type { AdminUser } from '@/api/types';
import { sam } from '@/test/fixtures';
import {
  assignBlocked,
  assignPatch,
  assignTargets,
  defaultAccess,
  displayName,
  householdOrder,
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

describe('householdOrder', () => {
  it('moves each member up to follow their owner, keeping the server order otherwise', () => {
    // The server lists by name: ellis and evie come before their owner.
    expect(ids(householdOrder([adam, bea, ellis, evie]))).toEqual(['adam', 'ellis', 'evie', 'bea']);
    expect(ids(householdOrder([ellis, bea, evie, adam]))).toEqual(['bea', 'adam', 'ellis', 'evie']);
  });

  it("keeps a member whose owner isn't in the list, in its own place", () => {
    expect(ids(householdOrder([bea, evie]))).toEqual(['bea', 'evie']);
    expect(ids(householdOrder([evie, bea]))).toEqual(['evie', 'bea']);
  });

  it('groups one level only: a member of a member keeps its own place', () => {
    const odd = memberOf(evie, 9, 'odd');
    expect(ids(householdOrder([odd, adam, evie]))).toEqual(['odd', 'adam', 'evie']);
  });

  it('shows everyone exactly once', () => {
    const all = [adam, bea, evie, ellis];
    expect(householdOrder(all)).toHaveLength(all.length);
    expect(new Set(householdOrder(all))).toEqual(new Set(all));
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
