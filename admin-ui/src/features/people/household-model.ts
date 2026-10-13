import type { AdminMember, AdminUser, MemberAccess, MemberKind, UserPatch } from '@/api/types';

// Household members in the console: how People groups them under their owner,
// who someone can be assigned to, and the bodies the Account tab sends. Pure,
// so the rules are tested apart from the screens.

/**
 * People in the order People shows them: the server's order, with each
 * person's household members moved up to follow them. A member whose owner
 * isn't in the list (filtered out, or not loaded) keeps its own place, so
 * hiding an owner never hides their members. One level only: a member is
 * grouped only under an owner who is not a member themselves.
 */
export function householdOrder(users: readonly AdminUser[]): AdminUser[] {
  const byId = new Map(users.map((u) => [u.id, u]));
  const ownerOf = (u: AdminUser) => {
    const owner = u.managed_by ? byId.get(u.managed_by.id) : undefined;
    return owner && !owner.managed_by && owner.id !== u.id ? owner : undefined;
  };
  const membersOf = new Map<number, AdminUser[]>();
  for (const u of users) {
    const owner = ownerOf(u);
    if (!owner) continue;
    const list = membersOf.get(owner.id);
    if (list) list.push(u);
    else membersOf.set(owner.id, [u]);
  }
  const out: AdminUser[] = [];
  for (const u of users) {
    if (ownerOf(u)) continue; // placed after their owner
    out.push(u, ...(membersOf.get(u.id) ?? []));
  }
  return out;
}

/** What to call a person: the name their household uses, else the username. */
export function displayName(u: Pick<AdminUser, 'username' | 'display_name'>): string {
  return u.display_name.trim() || u.username;
}

/**
 * The name a member's "Managed by" chip shows: the owner's display name when
 * the owner is among `users`, else the username the member's record carries.
 */
export function ownerName(u: AdminUser, users: readonly AdminUser[]): string | undefined {
  if (!u.managed_by) return undefined;
  const owner = users.find((o) => o.id === u.managed_by?.id);
  return owner ? displayName(owner) : u.managed_by.username;
}

/**
 * Why a person can't be put in someone's household (an i18n key under
 * user.household.blocked), or undefined when they can. The server refuses the
 * same cases (member_is_demo, member_not_admin, has_members).
 */
export function assignBlocked(u: AdminUser): 'admin' | 'hasMembers' | 'demo' | undefined {
  if (u.is_demo) return 'demo';
  if (u.role === 'admin') return 'admin';
  if (u.members_count > 0) return 'hasMembers';
  return undefined;
}

/**
 * Who `subject` can be assigned to: anyone else who isn't a household member
 * or a demo account (the server's owner_is_member, owner_is_demo and
 * self_managed), apart from the owner they already have.
 */
export function assignTargets(users: readonly AdminUser[], subject: AdminUser): AdminUser[] {
  return users.filter(
    (u) => u.id !== subject.id && !u.managed_by && !u.is_demo && u.id !== subject.managed_by?.id,
  );
}

/** The access a kind starts on: a kid on shared collections, a grown-up on everything. */
export function defaultAccess(kind: MemberKind): MemberAccess {
  return kind === 'kid' ? 'collections' : 'all';
}

/** "Assign to someone": the owner, with the kind and access the server requires beside it. */
export function assignPatch(ownerId: number, kind: MemberKind, access: MemberAccess): UserPatch {
  return { managed_by: ownerId, member_kind: kind, member_access: access };
}

/** "Make independent": clears all three member fields. */
export const independentPatch: UserPatch = { managed_by: null };

/**
 * The members a delete keeps (`?keep_members=`): every member of the person
 * not ticked to be deleted with them, in list order.
 */
export function keptMembers(
  members: readonly Pick<AdminMember, 'id'>[],
  deleting: ReadonlySet<number>,
): number[] {
  return members.filter((m) => !deleting.has(m.id)).map((m) => m.id);
}
