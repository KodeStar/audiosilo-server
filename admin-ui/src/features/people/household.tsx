import { useState } from 'react';
import { Link } from '@tanstack/react-router';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { FolderHeart, LibraryBig, Unlink, UserRoundPlus, type LucideIcon } from 'lucide-react';
import { api } from '@/api/client';
import { invalidatePeople, useSettings, useUser, useUsers } from '@/api/hooks';
import type { AdminMember, AdminUser, UserDetail } from '@/api/types';
import { ConfirmDialog } from '@/components/confirm-dialog';
import { Monogram } from '@/components/monogram';
import { SettingRow } from '@/components/setting-row';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Card, CardHeader } from '@/components/ui/card';
import { Checkbox } from '@/components/ui/checkbox';
import { Switch } from '@/components/ui/switch';
import { toastError } from '@/lib/errors';
import { formatNumber } from '@/lib/format';
import { toast } from '@/lib/toast';
import { AssignDialog } from './assign-dialog';
import { assignBlocked, independentPatch, ownerName } from './household-model';

// A person's household on their page: the Account tab's Household card, the
// Access tab's "through their owner" card, and the delete dialog's member list.

/** The name of a member's owner: their display name when People has loaded, else the username. */
function useOwnerName(u: AdminUser): string {
  const users = useUsers();
  return ownerName(u, users.data ?? []) ?? '';
}

/**
 * Account tab: for a household member, who manages them, with Make independent
 * and Assign to someone; for anyone else (not demo), the "Can add household
 * members" switch, their members, and Assign to someone.
 */
export function HouseholdCard({ detail }: { detail: UserDetail }) {
  const { t } = useTranslation();
  const u = detail.user;
  if (u.is_demo) return null;
  return (
    <Card aria-labelledby="household-title">
      <CardHeader
        titleId="household-title"
        title={t('user.household.title')}
        description={t('user.household.description')}
      />
      <div className="divide-y px-5">
        {u.managed_by ? <MemberRows user={u} /> : <OwnerRows detail={detail} />}
      </div>
    </Card>
  );
}

function MemberRows({ user: u }: { user: AdminUser }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const owner = useOwnerName(u);
  const [freeing, setFreeing] = useState(false);
  const [assigning, setAssigning] = useState(false);
  if (!u.managed_by) return null;
  return (
    <>
      <SettingRow
        title={t('user.household.managedBy')}
        description={t('user.household.managedByBody', { owner })}
      >
        <div className="flex flex-wrap items-center gap-2">
          <Link
            to="/people/user/$userId"
            params={{ userId: String(u.managed_by.id) }}
            className="font-semibold text-brand-ink hover:underline"
          >
            {owner}
          </Link>
          {u.member_kind ? (
            <Badge variant="secondary">{t(`people.kind.${u.member_kind}`)}</Badge>
          ) : null}
        </div>
      </SettingRow>
      <SettingRow
        title={t('user.household.independent')}
        description={t('user.household.independentBody', { name: u.username, owner })}
      >
        <Button variant="outline" onClick={() => setFreeing(true)}>
          <Unlink aria-hidden="true" />
          {t('user.household.independentAction')}
        </Button>
      </SettingRow>
      <SettingRow
        title={t('user.household.assign')}
        description={t('user.household.moveBody', { name: u.username })}
      >
        <Button variant="outline" onClick={() => setAssigning(true)}>
          <UserRoundPlus aria-hidden="true" />
          {t('user.household.assignAction')}
        </Button>
      </SettingRow>
      <ConfirmDialog
        open={freeing}
        onOpenChange={setFreeing}
        icon={Unlink}
        tone="info"
        destructive={false}
        title={t('user.household.independentTitle', { name: u.username })}
        description={t('user.household.independentDescription', { name: u.username, owner })}
        confirmLabel={t('user.household.independentAction')}
        onConfirm={async () => {
          await api.updateUser(u.id, independentPatch);
          invalidatePeople(qc);
          toast.add({
            title: t('user.household.independentDone', { name: u.username }),
            type: 'success',
          });
        }}
      >
        {u.member_kind === 'kid' ? (
          <p className="text-[13px] text-muted-foreground">
            {t('user.household.independentKid', { name: u.username })}
          </p>
        ) : null}
      </ConfirmDialog>
      <AssignDialog open={assigning} onOpenChange={setAssigning} user={u} />
    </>
  );
}

function OwnerRows({ detail }: { detail: UserDetail }) {
  const { t, i18n } = useTranslation();
  const qc = useQueryClient();
  const settings = useSettings();
  const [assigning, setAssigning] = useState(false);
  const u = detail.user;
  const members = detail.members ?? [];
  const household = settings.data?.household;
  const blocked = assignBlocked(u);

  const setCanAdd = async (on: boolean) => {
    try {
      await api.updateUser(u.id, { can_add_members: on });
      invalidatePeople(qc);
      toast.add({
        title: on
          ? t('user.household.canAddOn', { name: u.username })
          : t('user.household.canAddOff', { name: u.username }),
        description: on ? undefined : t('user.household.canAddOffBody'),
        type: 'success',
      });
    } catch (err) {
      toastError(t('user.account.failed'), err);
    }
  };

  return (
    <>
      <SettingRow
        title={t('user.household.canAdd')}
        htmlFor="can-add-members"
        descriptionId="can-add-members-desc"
        description={
          household && !household.members_enabled ? (
            <>
              {t('user.household.featureOff')}{' '}
              <Link
                to="/server/{-$section}"
                params={{ section: undefined }}
                search={{ topic: 'household' }}
                className="font-semibold text-brand-ink hover:underline"
              >
                {t('user.household.featureOffLink')}
              </Link>
            </>
          ) : (
            t('user.household.canAddBody', {
              name: u.username,
              max: formatNumber(household?.max_members ?? 6, i18n.resolvedLanguage ?? 'en'),
            })
          )
        }
      >
        <Switch
          id="can-add-members"
          checked={u.can_add_members}
          onCheckedChange={(on) => void setCanAdd(on)}
          aria-describedby="can-add-members-desc"
        />
      </SettingRow>
      <section aria-labelledby="household-members-title" className="flex flex-col gap-2 py-4">
        <h4 id="household-members-title" className="font-semibold">
          {t('user.household.members', { name: u.username })}
        </h4>
        {members.length === 0 ? (
          <p className="text-[12.5px] text-muted-foreground">{t('user.household.noMembers')}</p>
        ) : (
          <ul className="flex flex-col gap-1">
            {members.map((m) => (
              <MemberLink key={m.id} member={m} owner={u.username} />
            ))}
          </ul>
        )}
      </section>
      {blocked !== 'admin' ? (
        <SettingRow
          title={t('user.household.assign')}
          description={
            blocked
              ? t(`user.household.blocked.${blocked}`, { name: u.username })
              : t('user.household.assignBody', { name: u.username })
          }
        >
          <Button variant="outline" disabled={Boolean(blocked)} onClick={() => setAssigning(true)}>
            <UserRoundPlus aria-hidden="true" />
            {t('user.household.assignAction')}
          </Button>
        </SettingRow>
      ) : null}
      <AssignDialog open={assigning} onOpenChange={setAssigning} user={u} />
    </>
  );
}

/** One of the person's members, linking to their page. */
function MemberLink({ member: m, owner }: { member: AdminMember; owner: string }) {
  const { t } = useTranslation();
  return (
    <li>
      <Link
        to="/people/user/$userId"
        params={{ userId: String(m.id) }}
        className="-mx-2 flex min-w-0 items-center gap-3 rounded-md px-2 py-1.5 transition-colors duration-(--dur-1) hover:bg-accent"
      >
        <Monogram name={m.username} size={32} />
        <span className="flex min-w-0 flex-1 flex-col">
          <b className="truncate font-semibold">{m.name}</b>
          <span className="truncate text-[12.5px] text-muted-foreground">
            {m.name.toLowerCase() === m.username.toLowerCase()
              ? t(`user.household.accessShort.${m.access}`, { owner })
              : `${m.username} · ${t(`user.household.accessShort.${m.access}`, { owner })}`}
          </span>
        </span>
        <span className="flex shrink-0 flex-wrap justify-end gap-1.5">
          <Badge variant="secondary">{t(`people.kind.${m.kind}`)}</Badge>
          {m.disabled ? <Badge variant="outline">{t('user.disabled')}</Badge> : null}
        </span>
      </Link>
    </li>
  );
}

/**
 * Access tab, for a household member: where their books come from, read-only.
 * `all`: everything their owner can listen to (the owner's libraries); an
 * admin owner reaches every library. `collections`: the owner's collections
 * shared with them.
 */
export function MemberAccessCard({ detail }: { detail: UserDetail }) {
  const u = detail.user;
  if (!u.managed_by) return null;
  return u.member_access === 'collections' ? (
    <CollectionsAccess detail={detail} />
  ) : (
    <OwnerAccess user={u} />
  );
}

function OwnerAccess({ user: u }: { user: AdminUser }) {
  const { t } = useTranslation();
  const owner = useOwnerName(u);
  const ownerDetail = useUser(u.managed_by?.id ?? 0);
  const libs = ownerDetail.data?.accessible_libraries ?? [];
  const ownerIsAdmin = ownerDetail.data?.user.role === 'admin';
  return (
    <Card aria-labelledby="member-access-title">
      <CardHeader
        titleId="member-access-title"
        title={t('user.household.accessAll', { owner })}
        description={
          ownerIsAdmin
            ? t('user.household.accessAllAdmin', { name: u.username, owner })
            : t('user.household.accessAllBody', { name: u.username, owner })
        }
      />
      {ownerDetail.isError ? (
        <p className="px-5 pb-5 text-muted-foreground">{t('user.household.ownerUnknown')}</p>
      ) : !ownerDetail.data ? (
        <div className="px-5 pb-5">
          <span className="skel block h-12" role="status" aria-label={t('common.loading')} />
        </div>
      ) : libs.length === 0 ? (
        <p className="px-5 pb-5 text-muted-foreground">
          {t('user.household.ownerNothing', { owner })}
        </p>
      ) : (
        <ul className="divide-y border-t">
          {libs.map((l) => (
            <AccessRow key={l.id} icon={LibraryBig} title={l.name} />
          ))}
        </ul>
      )}
    </Card>
  );
}

function CollectionsAccess({ detail }: { detail: UserDetail }) {
  const { t, i18n } = useTranslation();
  const u = detail.user;
  const owner = useOwnerName(u);
  const collections = detail.member_collections ?? [];
  return (
    <Card aria-labelledby="member-access-title">
      <CardHeader
        titleId="member-access-title"
        title={t('user.household.accessCollections', { owner })}
        description={t('user.household.accessCollectionsBody', { name: u.username, owner })}
      />
      {collections.length === 0 ? (
        <p className="px-5 pb-5 text-muted-foreground">
          {t('user.household.noCollections', { name: u.username, owner })}
        </p>
      ) : (
        <ul className="divide-y border-t">
          {collections.map((c) => (
            <AccessRow
              key={c.id}
              icon={FolderHeart}
              title={c.name}
              detail={t('user.household.items', {
                count: c.items,
                formatted: formatNumber(c.items, i18n.resolvedLanguage ?? 'en'),
              })}
            />
          ))}
        </ul>
      )}
    </Card>
  );
}

function AccessRow({
  icon: Icon,
  title,
  detail,
}: {
  icon: LucideIcon;
  title: string;
  detail?: string;
}) {
  return (
    <li className="flex items-center gap-3 px-5 py-3">
      <span
        className="grid size-9 shrink-0 place-items-center rounded-[10px] bg-muted text-muted-foreground"
        aria-hidden="true"
      >
        <Icon className="size-[17px]" />
      </span>
      <div className="flex min-w-0 flex-1 flex-col">
        <b className="truncate font-semibold">{title}</b>
        {detail ? (
          <span className="truncate text-[12.5px] text-muted-foreground">{detail}</span>
        ) : null}
      </div>
    </li>
  );
}

/**
 * The delete dialog's member list: each of the person's household members,
 * ticked to be deleted with them. An unticked member is kept as an
 * independent account (`keep_members`).
 */
export function DeleteMembers({
  members,
  deleting,
  onChange,
}: {
  members: AdminMember[];
  deleting: ReadonlySet<number>;
  onChange: (next: Set<number>) => void;
}) {
  const { t } = useTranslation();
  if (members.length === 0) return null;
  const toggle = (id: number, on: boolean) => {
    const next = new Set(deleting);
    if (on) next.add(id);
    else next.delete(id);
    onChange(next);
  };
  return (
    <fieldset className="flex flex-col gap-2">
      <legend className="mb-1 text-[13px] font-semibold">
        {t('user.danger.members', { count: members.length })}
      </legend>
      <p className="text-[12.5px] text-muted-foreground">{t('user.danger.membersBody')}</p>
      {members.map((m) => (
        <label key={m.id} className="flex items-center gap-2.5 text-[13.5px]">
          <Checkbox
            checked={deleting.has(m.id)}
            onCheckedChange={(on) => toggle(m.id, Boolean(on))}
          />
          <span className="min-w-0 flex-1 truncate">
            {t('user.danger.deleteMember', { name: m.name })}
          </span>
          <Badge variant="secondary">{t(`people.kind.${m.kind}`)}</Badge>
        </label>
      ))}
    </fieldset>
  );
}
