import { useState } from 'react';
import { useNavigate, useParams, useSearch } from '@tanstack/react-router';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import {
  KeyRound,
  LibraryBig,
  LifeBuoy,
  Plus,
  QrCode,
  Share2,
  ShieldCheck,
  Smartphone,
  TriangleAlert,
  UserX,
} from 'lucide-react';
import { ApiError, api } from '@/api/client';
import { invalidatePeople, useDevices, useLibraries, useUser } from '@/api/hooks';
import type { User, UserDetail } from '@/api/types';
import { ConfirmDialog } from '@/components/confirm-dialog';
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
import { toastError } from '@/lib/errors';
import { formatRelative } from '@/lib/format';
import { useCurrentUser } from '@/lib/session';
import { toast } from '@/lib/toast';
import { GiveAccessDialog } from './give-access-dialog';
import { InviteDialog } from './invite-dialog';
import { InviteTable } from './invites-page';
import { PasswordDialog } from './password-dialog';
import { ListeningTab } from './user-listening';
import {
  ruleLabel,
  shareLabel,
  signedIn,
  sortInvites,
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

  const facts = [
    u.last_seen_at
      ? t('user.lastActive', { time: formatRelative(u.last_seen_at, lang) })
      : t('user.neverSignedIn'),
    u.has_password ? t('user.hasPassword') : t('user.noPassword'),
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
          <span className="text-muted-foreground">{facts.join(' · ')}</span>
        </div>
        <Button variant="outline" onClick={() => setInviting(true)} disabled={u.disabled}>
          <QrCode aria-hidden="true" />
          {t('user.pairDevice')}
        </Button>
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
          <DevicesTab user={u} devices={devices} onPair={() => setInviting(true)} />
        </TabsPanel>
        <TabsPanel value="invites">
          <InvitesTab user={u} detail={detail} onInvite={() => setInviting(true)} />
        </TabsPanel>
        <TabsPanel value="sign-in">
          <SignInTab user={u} />
        </TabsPanel>
        <TabsPanel value="account">
          <AccountTab user={u} />
        </TabsPanel>
      </Tabs>
      <InviteDialog open={inviting} onOpenChange={setInviting} user={u} codes={codes} />
    </Page>
  );
}

function DevicesTab({
  user,
  devices,
  onPair,
}: {
  user: User;
  devices: ReturnType<typeof useDevices>;
  onPair: () => void;
}) {
  const { t } = useTranslation();
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
  return (
    <div className="flex flex-col gap-5">
      {sessions.length ? (
        <DeviceList devices={sessions} showPerson={false} />
      ) : (
        <EmptyState
          icon={Smartphone}
          title={t('user.devices.emptyTitle')}
          body={t('user.devices.emptyBody', { name: user.username })}
          action={
            <Button onClick={onPair} disabled={user.disabled}>
              <QrCode aria-hidden="true" />
              {t('user.pairDevice')}
            </Button>
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
    <Card aria-labelledby="access-title">
      <CardHeader
        titleId="access-title"
        title={t('user.access.title', { name: u.username })}
        description={t('user.access.description')}
        action={
          <Button size="sm" variant="outline" onClick={() => setGiving(true)}>
            <Plus aria-hidden="true" />
            {t('user.access.give')}
          </Button>
        }
      />
      {grants.length === 0 ? (
        <p className="px-5 py-6 text-center text-muted-foreground">
          {t('user.access.none', { name: u.username })}
        </p>
      ) : (
        <ul className="divide-y">
          {grants.map((share) => {
            const library = wholeLibraryOf(share) !== undefined;
            const label = shareLabel(share, libs);
            const Icon = library ? LibraryBig : Share2;
            return (
              <li key={share.id} className="flex items-center gap-3 px-5 py-3">
                <span
                  className="grid size-9 shrink-0 place-items-center rounded-[10px] bg-muted text-muted-foreground"
                  aria-hidden="true"
                >
                  <Icon className="size-[17px]" />
                </span>
                <div className="flex min-w-0 flex-1 flex-col">
                  <b className="truncate font-semibold">{label}</b>
                  <span className="truncate text-[12.5px] text-muted-foreground">
                    {library
                      ? t('user.access.wholeLibrary')
                      : (share.paths ?? []).map((r) => ruleLabel(r, libs)).join(', ') ||
                        t('user.access.emptyShare')}
                  </span>
                </div>
                <Button
                  variant="ghost"
                  size="sm"
                  className="text-destructive"
                  onClick={() => void revoke(share.id, label)}
                  aria-label={t('user.access.removeAria', { what: label })}
                >
                  {t('user.access.remove')}
                </Button>
              </li>
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
  );
}

function InvitesTab({
  user,
  detail,
  onInvite,
}: {
  user: User;
  detail: UserDetail;
  onInvite: () => void;
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
        body={t('user.invites.emptyBody', { name: user.username })}
        action={
          <Button onClick={onInvite} disabled={user.disabled}>
            <QrCode aria-hidden="true" />
            {t('user.pairDevice')}
          </Button>
        }
      />
    );
  }
  return <InviteTable invites={invites} now={now} showFor={false} />;
}

function SignInTab({ user: u }: { user: User }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [password, setPassword] = useState(false);
  const [clearing, setClearing] = useState(false);
  return (
    <div className="grid grid-cols-1 items-start gap-4 lg:grid-cols-2">
      <Card className="flex flex-col gap-3 p-5" aria-labelledby="password-title">
        <h2 id="password-title" className="h3">
          {t('user.signIn.password')}
        </h2>
        <p className="text-[13.5px] text-muted-foreground">
          {u.has_password ? t('user.signIn.passwordSet') : t('user.signIn.passwordNone')}
        </p>
        <div>
          <Button size="sm" variant="outline" onClick={() => setPassword(true)}>
            <KeyRound aria-hidden="true" />
            {u.has_password ? t('user.signIn.change') : t('user.signIn.set')}
          </Button>
        </div>
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
      <PasswordDialog open={password} onOpenChange={setPassword} user={u} />
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

function AccountTab({ user: u }: { user: User }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const me = useCurrentUser();
  const navigate = useNavigate();
  const self = me.id === u.id;
  const [promoting, setPromoting] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [disabling, setDisabling] = useState(false);

  const patch = async (change: Parameters<typeof api.updateUser>[1], done: string) => {
    try {
      await api.updateUser(u.id, change);
      invalidatePeople(qc);
      toast.add({ title: done, type: 'success' });
    } catch (err) {
      toastError(t('user.account.failed'), err);
    }
  };

  const setRole = (role: User['role']) => {
    if (role === u.role) return;
    // An admin must have a password; ask for one in the same step.
    if (role === 'admin' && !u.has_password) {
      setPromoting(true);
      return;
    }
    const done = role === 'admin' ? 'user.account.nowAdmin' : 'user.account.nowMember';
    void patch({ role }, t(done, { name: u.username }));
  };

  return (
    <div className="flex max-w-[760px] flex-col gap-6">
      <Card className="px-5">
        <SettingRow
          title={t('user.account.role')}
          htmlFor="user-role"
          description={self ? t('user.account.roleSelf') : t('user.account.roleBody')}
        >
          <NativeSelect
            id="user-role"
            className="w-[180px]"
            value={u.role}
            disabled={self}
            onChange={(e) => setRole(e.target.value as User['role'])}
          >
            <option value="user">{t('people.role.user')}</option>
            <option value="admin">{t('people.role.admin')}</option>
          </NativeSelect>
        </SettingRow>
      </Card>

      <section
        aria-labelledby="danger-title"
        className="overflow-hidden rounded-xl border border-[color-mix(in_oklab,var(--destructive)_35%,var(--border))] bg-card"
      >
        <h2
          id="danger-title"
          className="flex items-center gap-2 border-b border-[color-mix(in_oklab,var(--destructive)_25%,var(--border))] bg-destructive-soft px-5 py-3 text-[14px] font-[650] text-destructive"
        >
          <TriangleAlert className="size-4" aria-hidden="true" />
          {t('user.danger.title')}
        </h2>
        <div className="divide-y px-5">
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
            <Button variant="destructive" disabled={self} onClick={() => setDeleting(true)}>
              <UserX aria-hidden="true" />
              {t('user.danger.deleteAction')}
            </Button>
          </SettingRow>
        </div>
      </section>

      <PasswordDialog open={promoting} onOpenChange={setPromoting} user={u} promote />
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
          await api.deleteUser(u.id);
          invalidatePeople(qc);
          toast.add({ title: t('user.danger.deleted', { name: u.username }), type: 'success' });
          void navigate({ to: '/people/{-$section}', params: { section: undefined } });
        }}
      >
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
