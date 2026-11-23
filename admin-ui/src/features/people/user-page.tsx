import { useState } from 'react';
import { useNavigate, useParams, useSearch } from '@tanstack/react-router';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import type { TFunction } from 'i18next';
import {
  CalendarClock,
  KeyRound,
  LibraryBig,
  LifeBuoy,
  LogOut,
  Plus,
  QrCode,
  Share2,
  ShieldCheck,
  Smartphone,
  UserX,
} from 'lucide-react';
import { ApiError, api } from '@/api/client';
import {
  forgetUsers,
  invalidateDevices,
  invalidatePeople,
  keys as queryKeys,
  useDevices,
  useLibraries,
  useUser,
} from '@/api/hooks';
import type { AdminUser, OIDCIdentity, User, UserDetail } from '@/api/types';
import { ConfirmDialog } from '@/components/confirm-dialog';
import { DangerZone } from '@/components/danger-zone';
import { EmptyState } from '@/components/empty-state';
import { SettingRow } from '@/components/setting-row';
import { Monogram } from '@/components/monogram';
import { Notice } from '@/components/notice';
import { Page } from '@/components/page';
import { QueryError } from '@/components/query-error';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Card, CardHeader } from '@/components/ui/card';
import { NativeSelect } from '@/components/ui/native-select';
import { Tabs, TabsList, TabsPanel, TabsTab } from '@/components/ui/tabs';
import { NotFound } from '@/features/not-found';
import { DeviceList } from './devices-page';
import { EmailCard } from './email-card';
import { IdentityCards } from './identity-card';
import { toastError } from '@/lib/errors';
import { formatRelative } from '@/lib/format';
import { useCurrentUser } from '@/lib/session';
import { toast } from '@/lib/toast';
import { AccessRow } from './access-row';
import { GiveAccessDialog } from './give-access-dialog';
import { DeleteMembers, HouseholdCard, MemberAccessCard } from './household';
import { deleteChoice, ownerName } from './household-model';
import { InviteDialog } from './invite-dialog';
import { InviteTable } from './invites-page';
import { PasswordDialog } from './password-dialog';
import { MemberChips } from './member-chips';
import { signInProvider } from './pending-model';
import { ListeningTab } from './user-listening';
import {
  endedKeysToast,
  endKeysWhat,
  ruleLabel,
  shareLabel,
  signedIn,
  sortInvites,
  grantSummary,
  wholeLibraryOf,
  type UserTab,
} from './people-model';

/**
 * One person: their listening (year, progress, sessions), what they can listen to,
 * their devices and invites, how they sign in, and their account.
 */
export function UserPage() {
  const { t } = useTranslation();
  const { userId } = useParams({ from: '/people/user/$userId' });
  const id = Number(userId);
  const detail = useUser(id);

  if (!Number.isInteger(id) || id <= 0) return <NotFound />;
  if (detail.isError) {
    if (detail.error instanceof ApiError && detail.error.status === 404) return <NotFound />;
    return (
      <Page>
        <QueryError
          title={t('user.error')}
          error={detail.error}
          onRetry={() => void detail.refetch()}
        />
      </Page>
    );
  }
  if (!detail.data) {
    return (
      <Page>
        <div className="flex flex-col gap-6" role="status" aria-label={t('common.loading')}>
          <div className="skel h-[84px] w-[min(420px,100%)] rounded-xl" />
          <div className="skel h-[260px] rounded-xl" />
        </div>
      </Page>
    );
  }
  return <UserView detail={detail.data} />;
}

function UserView({ detail }: { detail: UserDetail }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const u = detail.user;
  const codes = detail.auth_codes ?? [];
  const search = useSearch({ from: '/people/user/$userId' });
  const navigate = useNavigate({ from: '/people/user/$userId' });
  const tab: UserTab = search.tab ?? 'listening';
  const devices = useDevices(u.id);
  const [inviting, setInviting] = useState(false);
  // A kid holds no invite: only their owner sets up their devices, from the
  // player (the server answers 409 kid_account), so the page offers no pairing.
  const kid = u.member_kind === 'kid';
  const identities = detail.identities ?? [];
  // The provider they sign in with, when one of their single sign-ons is the
  // one the server runs: said in place of "player only", and, like a password,
  // an admin's way into the console. The server says (sign_in_provider): never
  // for a household member, who keeps an identity linked before they joined
  // but whom single sign-on never signs in (auth.ConsumeOIDC), nor a demo
  // account.
  const sso = signInProvider(u);

  const facts = [
    u.last_seen_at
      ? t('user.lastActive', { time: formatRelative(u.last_seen_at, lang) })
      : t('user.neverSignedIn'),
    u.has_password
      ? t('user.hasPassword')
      : sso
        ? t('user.noPasswordSSO', { provider: sso })
        : t('user.noPassword'),
  ];

  return (
    <Page>
      <div className="mb-6 flex flex-wrap items-center gap-x-5 gap-y-4">
        <Monogram name={u.username} size={72} />
        <div className="flex min-w-0 flex-1 basis-60 flex-col gap-1.5">
          <div className="flex flex-wrap items-center gap-2.5">
            <h1 className="display [overflow-wrap:anywhere]">{u.username}</h1>
            <Badge variant={u.role === 'admin' ? 'ink' : 'secondary'}>
              {t(`people.role.${u.role}`)}
            </Badge>
            {u.disabled ? <Badge variant="outline">{t('user.disabled')}</Badge> : null}
            {u.is_demo ? <Badge variant="outline">{t('people.badge.demo')}</Badge> : null}
          </div>
          <MemberChips user={u} />
          <span className="text-muted-foreground">{facts.join(' · ')}</span>
        </div>
        {kid ? null : (
          <Button variant="outline" onClick={() => setInviting(true)} disabled={u.disabled}>
            <QrCode aria-hidden="true" />
            {t('user.pairDevice')}
          </Button>
        )}
      </div>

      <Tabs
        value={tab}
        onValueChange={(v) =>
          void navigate({ search: v === 'listening' ? {} : { tab: v as UserTab }, replace: true })
        }
      >
        <TabsList className="mb-6" aria-label={t('user.tabs')}>
          <TabsTab value="listening">{t('user.tab.listening')}</TabsTab>
          <TabsTab value="access">{t('user.tab.access')}</TabsTab>
          <TabsTab value="devices" count={devices.data && signedIn(devices.data).length}>
            {t('user.tab.devices')}
          </TabsTab>
          <TabsTab value="invites" count={codes.length}>
            {t('user.tab.invites')}
          </TabsTab>
          <TabsTab value="sign-in">{t('user.tab.signIn')}</TabsTab>
          <TabsTab value="account">{t('user.tab.account')}</TabsTab>
        </TabsList>
        <TabsPanel value="listening">
          <ListeningTab user={u} />
        </TabsPanel>
        <TabsPanel value="access">
          <AccessTab detail={detail} />
        </TabsPanel>
        <TabsPanel value="devices">
          <DevicesTab
            user={u}
            devices={devices}
            sso={sso}
            calendarLink={detail.calendar_link}
            onPair={kid ? undefined : () => setInviting(true)}
          />
        </TabsPanel>
        <TabsPanel value="invites">
          <InvitesTab
            user={u}
            detail={detail}
            onInvite={kid ? undefined : () => setInviting(true)}
          />
        </TabsPanel>
        <TabsPanel value="sign-in">
          <SignInTab user={u} owner={ownerName(u) ?? ''} identities={identities} sso={sso} />
        </TabsPanel>
        <TabsPanel value="account">
          <AccountTab detail={detail} sso={sso} />
        </TabsPanel>
      </Tabs>
      {kid ? null : (
        <InviteDialog open={inviting} onOpenChange={setInviting} user={u} codes={codes} />
      )}
    </Page>
  );
}

/**
 * Signs a person out of every device (server#281): their sessions end, and with
 * them anything they are casting, their open pairing codes and their recovery
 * code, and their household members' sign-ins, API keys and calendar links made
 * on those devices; their own API keys and calendar links stay. Offered while they have a session the
 * console isn't using. The words say how they get back in: a password, their
 * single sign-on (sso, its provider's name) or a new invite, or a kid's owner.
 */
function SignOutEverywhere({ user, sso }: { user: AdminUser; sso?: string }) {
  const { t } = useTranslation();
  const self = useCurrentUser().id === user.id;
  const qc = useQueryClient();
  const [open, setOpen] = useState(false);
  const description = self
    ? t('user.devices.signOutAllSelfBody')
    : user.member_kind === 'kid'
      ? t('user.devices.signOutAllKidBody', { name: user.username, owner: ownerName(user) ?? '' })
      : user.has_password
        ? t('user.devices.signOutAllBody', { name: user.username })
        : sso
          ? t('user.devices.signOutAllSSOBody', { name: user.username, provider: sso })
          : t('user.devices.signOutAllPairedBody', { name: user.username });
  return (
    <>
      <Button
        size="sm"
        variant="destructive-outline"
        className="self-start"
        onClick={() => setOpen(true)}
      >
        <LogOut aria-hidden="true" />
        {t('user.devices.signOutAll')}
      </Button>
      <ConfirmDialog
        open={open}
        onOpenChange={setOpen}
        icon={LogOut}
        title={t('user.devices.signOutAllTitle', { name: user.username })}
        description={description}
        confirmLabel={t('user.devices.signOutAll')}
        onConfirm={async () => {
          const { signed_out } = await api.signOutEverywhere(user.id);
          invalidateDevices(qc);
          invalidatePeople(qc); // the recovery code is gone too
          toast.add({
            title: t('user.devices.signedOutAll', { count: signed_out, name: user.username }),
            type: 'success',
          });
        }}
      />
    </>
  );
}

/**
 * Ends a person's API keys and their calendar link at once (server#285), the
 * lever Sign out everywhere leaves out: their sessions stay. Offered while they
 * have either, its button, title and body naming only what they have; the toast
 * says what it ended.
 */
function EndKeys({ user, keys, link }: { user: AdminUser; keys: number; link: boolean }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [open, setOpen] = useState(false);
  const self = useCurrentUser().id === user.id;
  const what = endKeysWhat(keys, link);
  return (
    <>
      <Button
        size="sm"
        variant="destructive-outline"
        className="self-start"
        onClick={() => setOpen(true)}
      >
        <KeyRound aria-hidden="true" />
        {t(`user.devices.endKeys.${what}`)}
      </Button>
      <ConfirmDialog
        open={open}
        onOpenChange={setOpen}
        icon={KeyRound}
        title={t(`user.devices.endKeys.${self ? 'selfTitle' : 'title'}.${what}`, {
          name: user.username,
          count: keys,
        })}
        description={t(`user.devices.endKeys.${self ? 'selfBody' : 'body'}.${what}`, {
          name: user.username,
        })}
        confirmLabel={t(`user.devices.endKeys.${what}`)}
        onConfirm={async () => {
          const ended = await api.endKeys(user.id);
          invalidateDevices(qc);
          // The detail alone (its calendar link), not the keys under it (the listening).
          void qc.invalidateQueries({ queryKey: queryKeys.user(user.id), exact: true });
          toast.add({ title: endedKeysToast(t, ended, user.username, self), type: 'success' });
        }}
      />
    </>
  );
}

/**
 * A person's devices; onPair is undefined for a kid, whose owner pairs them from
 * the player. sso names the single sign-on they sign in with, if any; calendarLink
 * whether they have a calendar link.
 */
function DevicesTab({
  user,
  devices,
  sso,
  calendarLink,
  onPair,
}: {
  user: AdminUser;
  devices: ReturnType<typeof useDevices>;
  sso?: string;
  calendarLink?: UserDetail['calendar_link'];
  onPair?: () => void;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  if (devices.isError) {
    return (
      <QueryError
        title={t('devices.error')}
        error={devices.error}
        onRetry={() => void devices.refetch()}
      />
    );
  }
  if (!devices.data) return <div className="skel h-24 rounded-xl" />;
  // Devices first, as the tab counts them and the person card lists them; the
  // person's API keys follow under their own heading.
  const sessions = signedIn(devices.data);
  const keys = devices.data.filter((d) => d.kind === 'api');
  const link = calendarLink?.active ?? false;
  return (
    <div className="flex flex-col gap-5">
      {sessions.length ? (
        <div className="flex flex-col gap-3">
          <DeviceList devices={sessions} showPerson={false} />
          {sessions.some((d) => !d.current) ? <SignOutEverywhere user={user} sso={sso} /> : null}
        </div>
      ) : (
        <EmptyState
          icon={Smartphone}
          title={t('user.devices.emptyTitle')}
          body={
            onPair
              ? t('user.devices.emptyBody', { name: user.username })
              : t('user.kidDevices', { owner: ownerName(user) ?? '' })
          }
          action={
            onPair ? (
              <Button onClick={onPair} disabled={user.disabled}>
                <QrCode aria-hidden="true" />
                {t('user.pairDevice')}
              </Button>
            ) : undefined
          }
        />
      )}
      {keys.length ? (
        <section aria-labelledby="user-keys-title" className="flex flex-col gap-2.5">
          <h2 id="user-keys-title" className="eyebrow">
            {t('user.devices.keys', { count: keys.length })}
          </h2>
          <DeviceList devices={keys} showPerson={false} />
        </section>
      ) : null}
      {link ? (
        <section aria-labelledby="user-link-title" className="flex flex-col gap-1.5">
          <h2 id="user-link-title" className="eyebrow">
            {t('user.devices.calendarLink')}
          </h2>
          <p className="flex items-start gap-2 text-[13.5px] text-muted-foreground">
            <CalendarClock className="mt-0.5 size-4 shrink-0" aria-hidden="true" />
            {calendarLink?.created_at
              ? t('user.devices.calendarLinkSince', {
                  when: formatRelative(calendarLink.created_at, lang),
                })
              : t('user.devices.calendarLinkOn')}
          </p>
        </section>
      ) : null}
      {keys.length || link ? <EndKeys user={user} keys={keys.length} link={link} /> : null}
    </div>
  );
}

function AccessTab({ detail }: { detail: UserDetail }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const libraries = useLibraries();
  const [giving, setGiving] = useState(false);
  const u = detail.user;
  const libs = libraries.data ?? [];

  if (u.role === 'admin') {
    return (
      <Notice tone="info" icon={ShieldCheck}>
        {t('user.access.admin')}
      </Notice>
    );
  }
  const grants = detail.shares ?? [];
  const member = Boolean(u.managed_by);

  const revoke = async (shareId: number, label: string) => {
    try {
      await api.revokeShare(u.id, shareId);
      invalidatePeople(qc);
      toast.add({
        title: t('user.access.removed', { what: label }),
        description: t('user.access.changedBody', { name: u.username }),
        type: 'success',
      });
    } catch (err) {
      toastError(t('user.access.failed'), err);
    }
  };

  return (
    <div className="flex flex-col gap-5">
      <MemberAccessCard detail={detail} />
      <Card aria-labelledby="access-title">
        <CardHeader
          titleId="access-title"
          title={
            member ? t('user.access.directTitle') : t('user.access.title', { name: u.username })
          }
          description={
            member
              ? t('user.access.directDescription', { name: u.username })
              : t('user.access.description')
          }
          action={
            <Button size="sm" variant="outline" onClick={() => setGiving(true)}>
              <Plus aria-hidden="true" />
              {t('user.access.give')}
            </Button>
          }
        />
        {grants.length === 0 ? (
          <p className="px-5 py-6 text-center text-muted-foreground">
            {member ? t('user.access.noneDirect') : t('user.access.none', { name: u.username })}
          </p>
        ) : (
          <ul className="divide-y">
            {grants.map((share) => {
              const library = wholeLibraryOf(share) !== undefined;
              const label = shareLabel(share, t, libs);
              return (
                <AccessRow
                  key={share.id}
                  icon={library ? LibraryBig : Share2}
                  title={label}
                  detail={
                    library
                      ? grantSummary(share, libs, t)
                      : (share.paths ?? []).map((r) => ruleLabel(r, libs)).join(', ') ||
                        t('user.access.emptyShare')
                  }
                  action={
                    <Button
                      variant="ghost"
                      size="sm"
                      className="text-destructive"
                      onClick={() => void revoke(share.id, label)}
                      aria-label={t('user.access.removeAria', { what: label })}
                    >
                      {t('user.access.remove')}
                    </Button>
                  }
                />
              );
            })}
          </ul>
        )}
        <GiveAccessDialog
          open={giving}
          onOpenChange={setGiving}
          user={u}
          granted={detail.shares ?? []}
        />
      </Card>
    </div>
  );
}

/**
 * A person's invites. onInvite is undefined for a kid: their owner sets up their
 * devices, so there is no invite to make, and the used-up or expired ones kept
 * as history can't be rotated back to life.
 */
function InvitesTab({
  user,
  detail,
  onInvite,
}: {
  user: AdminUser;
  detail: UserDetail;
  onInvite?: () => void;
}) {
  const { t } = useTranslation();
  const now = Date.now();
  const invites = sortInvites(
    (detail.auth_codes ?? []).map((c) => ({ ...c, user_id: user.id, username: user.username })),
    now,
  );
  if (invites.length === 0) {
    return (
      <EmptyState
        icon={Smartphone}
        title={t('user.invites.emptyTitle')}
        body={
          onInvite
            ? t('user.invites.emptyBody', { name: user.username })
            : t('user.kidDevices', { owner: ownerName(user) ?? '' })
        }
        action={
          onInvite ? (
            <Button onClick={onInvite} disabled={user.disabled}>
              <QrCode aria-hidden="true" />
              {t('user.pairDevice')}
            </Button>
          ) : undefined
        }
      />
    );
  }
  return <InviteTable invites={invites} now={now} showFor={false} canRotate={Boolean(onInvite)} />;
}

/**
 * How a person signs in. A kid holds no password (the server refuses one), so
 * their card says how they do sign in instead, as it does for someone without
 * one who signs in with single sign-on (sso, its provider's name). A household
 * member has no email card: members never sign in with single sign-on. Each
 * linked single sign-on has a card of its own, with Unlink.
 */
function SignInTab({
  user: u,
  owner,
  identities,
  sso,
}: {
  user: AdminUser;
  owner: string;
  identities: OIDCIdentity[];
  sso?: string;
}) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [password, setPassword] = useState(false);
  const [clearing, setClearing] = useState(false);
  const kid = u.member_kind === 'kid';
  return (
    <div className="grid grid-cols-1 items-start gap-4 lg:grid-cols-2">
      <Card className="flex flex-col gap-3 p-5" aria-labelledby="password-title">
        <h2 id="password-title" className="h3">
          {t('user.signIn.password')}
        </h2>
        <p className="text-[13.5px] text-muted-foreground">
          {kid
            ? t('user.signIn.passwordKid', { owner })
            : u.has_password
              ? t('user.signIn.passwordSet')
              : sso
                ? t('user.signIn.passwordNoneSSO', { provider: sso })
                : t('user.signIn.passwordNone')}
        </p>
        {kid ? null : (
          <div>
            <Button size="sm" variant="outline" onClick={() => setPassword(true)}>
              <KeyRound aria-hidden="true" />
              {u.has_password ? t('user.signIn.change') : t('user.signIn.set')}
            </Button>
          </div>
        )}
      </Card>
      {u.has_recovery ? (
        <Card className="flex flex-col gap-3 p-5" aria-labelledby="recovery-title">
          <h2 id="recovery-title" className="h3">
            {t('user.signIn.recovery')}
          </h2>
          <p className="text-[13.5px] text-muted-foreground">
            {t('user.signIn.recoveryBody', { name: u.username })}
          </p>
          <div>
            <Button size="sm" variant="destructive-outline" onClick={() => setClearing(true)}>
              <LifeBuoy aria-hidden="true" />
              {t('user.signIn.revokeRecovery')}
            </Button>
          </div>
        </Card>
      ) : null}
      {identities.length ? <IdentityCards user={u} identities={identities} /> : null}
      {u.managed_by ? null : <EmailCard key={u.email} user={u} />}
      {kid ? null : <PasswordDialog open={password} onOpenChange={setPassword} user={u} />}
      <ConfirmDialog
        open={clearing}
        onOpenChange={setClearing}
        icon={LifeBuoy}
        title={t('user.signIn.revokeTitle', { name: u.username })}
        description={t('user.signIn.revokeBody')}
        confirmLabel={t('user.signIn.revokeRecovery')}
        onConfirm={async () => {
          await api.clearRecovery(u.id);
          invalidatePeople(qc);
          toast.add({ title: t('user.signIn.revoked'), type: 'success' });
        }}
      />
    </div>
  );
}

/** The account: role, household and the danger zone. sso: see SignInTab. */
function AccountTab({ detail, sso }: { detail: UserDetail; sso?: string }) {
  const { t } = useTranslation();
  const u = detail.user;
  const members = detail.members ?? [];
  const qc = useQueryClient();
  const me = useCurrentUser();
  const navigate = useNavigate();
  const self = me.id === u.id;
  const [promoting, setPromoting] = useState(false);
  const [deleting, setDeleting] = useState(false);
  // The members the delete keeps (unticked): none until one is unticked.
  const [keeping, setKeeping] = useState<Set<number>>(() => new Set());
  const [disabling, setDisabling] = useState(false);
  // A role change for a person with household members, waiting for the
  // confirm that says what changes for them (server#222). The role stays set
  // while the dialog closes, so its words don't change on the way out.
  const [ownerRole, setOwnerRole] = useState<User['role']>('user');
  const [confirmingRole, setConfirmingRole] = useState(false);
  const member = Boolean(u.managed_by);

  const patch = async (change: Parameters<typeof api.updateUser>[1], done: string) => {
    try {
      await api.updateUser(u.id, change);
      invalidatePeople(qc);
      toast.add({ title: done, type: 'success' });
    } catch (err) {
      toastError(t('user.account.failed'), err);
      // Made an admin on their single sign-on, which the server no longer
      // counts (turned off since the page read it): ask for a password.
      if (err instanceof ApiError && err.code === 'admin_needs_password') {
        // The person as read (sign_in_provider) is stale.
        invalidatePeople(qc);
        setPromoting(true);
      }
    }
  };

  const setRole = (role: User['role']) => {
    if (role === u.role) return;
    // Members take their access from their owner (an admin owner gives every
    // library), so a change of the owner's role changes what they see too.
    if (members.length) {
      setOwnerRole(role);
      setConfirmingRole(true);
      return;
    }
    void applyRole(role);
  };

  // Resolves once the change is made (or has said why it wasn't), so the
  // owner-role confirm stays busy until then.
  const applyRole = async (role: User['role']) => {
    // An admin needs a way into the console (owner decision 5): a password, or
    // a single sign-on from the provider the server runs. With neither, ask for
    // a password in the same step; the server's answer stays the authority.
    if (role === 'admin' && !u.has_password && !sso) {
      setPromoting(true);
      return;
    }
    const done = role === 'admin' ? 'user.account.nowAdmin' : 'user.account.nowListener';
    await patch({ role }, t(done, { name: u.username }));
  };

  return (
    <div className="flex max-w-[760px] flex-col gap-6">
      <Card className="px-5">
        <SettingRow
          title={t('user.account.role')}
          htmlFor="user-role"
          description={
            self
              ? t('user.account.roleSelf')
              : member
                ? t('user.account.roleMember', { name: u.username, owner: ownerName(u) })
                : t('user.account.roleBody')
          }
        >
          <NativeSelect
            id="user-role"
            className="w-[180px]"
            value={u.role}
            disabled={self}
            onChange={(e) => setRole(e.target.value as User['role'])}
          >
            <option value="user">{t('people.role.user')}</option>
            {/* A household member can't be an admin (the server answers 409 member_not_admin). */}
            <option value="admin" disabled={member && u.role !== 'admin'}>
              {t('people.role.admin')}
            </option>
          </NativeSelect>
        </SettingRow>
      </Card>

      <HouseholdCard detail={detail} />

      <DangerZone>
        <SettingRow
          title={u.disabled ? t('user.danger.enable') : t('user.danger.disable')}
          description={
            self
              ? t('user.danger.disableSelf')
              : u.disabled
                ? t('user.danger.enableBody')
                : t('user.danger.disableBody')
          }
        >
          {u.disabled ? (
            <Button
              variant="outline"
              onClick={() =>
                void patch({ disabled: false }, t('user.danger.enabled', { name: u.username }))
              }
            >
              {t('user.danger.enableAction')}
            </Button>
          ) : (
            <Button
              variant="destructive-outline"
              disabled={self}
              onClick={() => setDisabling(true)}
            >
              {t('user.danger.disableAction')}
            </Button>
          )}
        </SettingRow>
        <SettingRow
          title={t('user.danger.delete')}
          description={
            self ? t('user.danger.deleteSelf') : t('user.danger.deleteBody', { name: u.username })
          }
        >
          <Button
            variant="destructive"
            disabled={self}
            onClick={() => {
              setKeeping(new Set());
              setDeleting(true);
            }}
          >
            <UserX aria-hidden="true" />
            {t('user.danger.deleteAction')}
          </Button>
        </SettingRow>
      </DangerZone>

      <PasswordDialog open={promoting} onOpenChange={setPromoting} user={u} promote />
      <ConfirmDialog
        open={confirmingRole}
        onOpenChange={setConfirmingRole}
        icon={ShieldCheck}
        tone="info"
        destructive={false}
        title={t('user.account.roleOwnerTitle', { name: u.username })}
        description={t(
          ownerRole === 'admin' ? 'user.account.roleOwnerAdmin' : 'user.account.roleOwnerListener',
          { name: u.username },
        )}
        confirmLabel={t('user.account.roleOwnerConfirm')}
        onConfirm={() => applyRole(ownerRole)}
      />
      <ConfirmDialog
        open={disabling}
        onOpenChange={setDisabling}
        icon={UserX}
        title={t('user.danger.disableTitle', { name: u.username })}
        description={t('user.danger.disableBody')}
        confirmLabel={t('user.danger.disableAction')}
        onConfirm={async () => {
          await api.updateUser(u.id, { disabled: true });
          invalidatePeople(qc);
          toast.add({ title: t('user.danger.disabled', { name: u.username }), type: 'success' });
        }}
      />
      <ConfirmDialog
        open={deleting}
        onOpenChange={setDeleting}
        icon={UserX}
        title={t('user.danger.deleteTitle', { name: u.username })}
        description={t('user.danger.deleteDescription')}
        confirmLabel={t('user.danger.deleteAction')}
        typeToConfirm={u.username}
        onConfirm={async () => {
          const { keep, remove } = deleteChoice(members, keeping);
          const outcome = await api.deleteUser(u.id, remove, keep);
          toast.add({
            title: t('user.danger.deleted', { name: u.username }),
            description: outcome
              ? membersOutcome(outcome.members_deleted, outcome.members_kept, t)
              : undefined,
            type: 'success',
          });
          // Off their page first: refreshing it (or any page of theirs) now
          // would only meet a 404.
          await navigate({ to: '/people/{-$section}', params: { section: undefined } });
          forgetUsers(qc, [u.id, ...remove]);
          invalidatePeople(qc);
        }}
      >
        <DeleteMembers members={members} keeping={keeping} onChange={setKeeping} />
        {!u.disabled ? (
          <p className="text-[13px] text-muted-foreground">
            {t('user.danger.reversible')}{' '}
            <button
              type="button"
              className="font-semibold text-brand-ink hover:underline"
              onClick={() => {
                setDeleting(false);
                setDisabling(true);
              }}
            >
              {t('user.danger.disableInstead')}
            </button>
          </p>
        ) : null}
      </ConfirmDialog>
    </div>
  );
}

/** What a delete did to the person's household members, for its toast (undefined: they had none). */
function membersOutcome(deleted: number, kept: number, t: TFunction): string | undefined {
  const parts = [
    deleted ? t('user.danger.deletedMembers', { count: deleted }) : '',
    kept ? t('user.danger.keptMembers', { count: kept }) : '',
  ].filter(Boolean);
  return parts.length ? parts.join(' ') : undefined;
}
