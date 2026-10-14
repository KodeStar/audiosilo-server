import type { AdminMember, AdminUser, MemberAccess, MemberKind, UserPatch } from '@/api/types';

// Household members in the console: how People groups them under their owner,
// who someone can be assigned to, and the bodies the Account tab sends. Pure,
// so the rules are tested apart from the screens.

/** One entry on People: an ordinary person's card, or a household shown as one block. */
export type PeopleEntry =
  | { kind: 'person'; user: AdminUser }
  | { kind: 'household'; owner: AdminUser; members: AdminUser[] };

/**
 * People as People shows them: the server's order, with each household (an
 * owner and the members listed with them) gathered into one entry where the
 * owner stands. A member whose owner isn't in the list (filtered out, or not
 * loaded) keeps its own place as a person, so hiding an owner never hides
 * their members; an owner with none of their members listed is a person too.
 * One level only: a member is grouped only under an owner who is not a member
 * themselves.
 */
export function peopleGroups(users: readonly AdminUser[]): PeopleEntry[] {
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
  const out: PeopleEntry[] = [];
  for (const u of users) {
    if (ownerOf(u)) continue; // inside their owner's household
    const members = membersOf.get(u.id);
    out.push(members ? { kind: 'household', owner: u, members } : { kind: 'person', user: u });
  }
  return out;
}

/** What to call a person: the name their household uses, else the username. */
export function displayName(u: Pick<AdminUser, 'username' | 'display_name'>): string {
  return u.display_name.trim() || u.username;
}

/**
 * The name a member's owner goes by (the record's managed_by carries the
 * owner's display name), undefined for anyone who isn't a member.
 */
export function ownerName(u: Pick<AdminUser, 'managed_by'>): string | undefined {
  return u.managed_by ? displayName(u.managed_by) : undefined;
}

/**
 * Whether a household name says more than the username beside it: it differs
 * other than in case ("Evie" adds nothing to "evie").
 */
export function namedApart(name: string, username: string): boolean {
  return name.trim().toLowerCase() !== username.toLowerCase();
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
 * The members a delete keeps (`?keep_members=`): those of the person's members
 * unticked to stay as independent accounts, in list order (an id that isn't one
 * of theirs is never sent).
 */
export function keptMembers(
  members: readonly Pick<AdminMember, 'id'>[],
  keeping: ReadonlySet<number>,
): number[] {
  return members.filter((m) => keeping.has(m.id)).map((m) => m.id);
}
