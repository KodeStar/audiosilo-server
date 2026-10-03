import { useState } from 'react';
import { Link } from '@tanstack/react-router';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { MailX, RefreshCw, Ticket, UserPlus } from 'lucide-react';
import { api } from '@/api/client';
import { invalidatePeople, useInvites } from '@/api/hooks';
import type { Invite } from '@/api/types';
import { ConfirmDialog } from '@/components/confirm-dialog';
import { EmptyState } from '@/components/empty-state';
import { Monogram } from '@/components/monogram';
import { Page } from '@/components/page';
import { PageHead } from '@/components/page-head';
import { QueryError } from '@/components/query-error';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Dialog } from '@/components/ui/dialog';
import { toastError } from '@/lib/errors';
import { SegmentedControl } from '@/components/ui/segmented-control';
import { formatRelative } from '@/lib/format';
import { toast } from '@/lib/toast';
import { InviteDialog, InviteResultContent, type ShownInvite } from './invite-dialog';
import { inviteStatus, sortInvites, type InviteStatus } from './people-model';

/** People > Invites: every invite link, who it's for, and how much of it is left. */
export function InvitesPage() {
  const { t } = useTranslation();
  const invites = useInvites();
  const [all, setAll] = useState(false);
  const [inviting, setInviting] = useState(false);
  const now = Date.now();

  const sorted = sortInvites(invites.data ?? [], now);
  const activeCount = sorted.filter((i) => inviteStatus(i, now) === 'active').length;
  const shown = all ? sorted : sorted.filter((i) => inviteStatus(i, now) === 'active');

  const inviteButton = (
    <Button onClick={() => setInviting(true)}>
      <UserPlus aria-hidden="true" />
      {t('people.invite')}
    </Button>
  );

  return (
    <Page>
      <PageHead
        title={t('invites.title')}
        description={t('invites.description')}
        action={inviteButton}
      />
      {invites.isError ? (
        <QueryError
          title={t('invites.error')}
          error={invites.error}
          onRetry={() => void invites.refetch()}
        />
      ) : !invites.data ? (
        <div className="skel h-[220px] rounded-xl" role="status" aria-label={t('common.loading')} />
      ) : sorted.length === 0 ? (
        <EmptyState
          icon={Ticket}
          title={t('invites.empty.title')}
          body={t('invites.empty.body')}
          action={inviteButton}
        />
      ) : (
        <>
          <SegmentedControl
            className="mb-3.5"
            label={t('invites.filter')}
            value={all}
            onChange={setAll}
            options={[
              { value: false, label: t('invites.showActive', { count: activeCount }) },
              { value: true, label: t('invites.showAll', { count: sorted.length }) },
            ]}
          />
          {shown.length === 0 ? (
            <p className="rounded-xl border bg-card px-5 py-6 text-center text-muted-foreground">
              {t('invites.noneActive')}
            </p>
          ) : (
            <InviteTable invites={shown} now={now} />
          )}
        </>
      )}
      <InviteDialog open={inviting} onOpenChange={setInviting} />
    </Page>
  );
}

const STATUS_BADGE: Record<InviteStatus, 'success' | 'outline' | 'warning'> = {
  active: 'success',
  usedUp: 'outline',
  expired: 'warning',
};

/** The invite list, shared with a person's own Invites tab (`showFor` off). */
export function InviteTable({
  invites,
  now,
  showFor = true,
}: {
  invites: Invite[];
  now: number;
  showFor?: boolean;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const qc = useQueryClient();
  const [rotated, setRotated] = useState<ShownInvite>();
  const [revoking, setRevoking] = useState<Invite>();

  const rotate = async (inv: Invite) => {
    try {
      setRotated({ ...(await api.rotateInvite(inv.id)), name: inv.username });
      invalidatePeople(qc);
    } catch (err) {
      toastError(t('invites.toast.rotateFailed'), err);
    }
  };

  return (
    <div className="overflow-x-auto rounded-xl border bg-card">
      <table className="w-full text-left text-[13.5px]">
        <thead className="border-b text-[12px] text-muted-foreground">
          <tr>
            {showFor ? <th className="px-4 py-2.5 font-medium">{t('invites.col.for')}</th> : null}
            <th className="px-4 py-2.5 font-medium">{t('invites.col.status')}</th>
            <th className="px-4 py-2.5 text-right font-medium max-md:hidden">
              {t('invites.col.devices')}
            </th>
            <th className="px-4 py-2.5 font-medium max-md:hidden">{t('invites.col.expires')}</th>
            <th className="px-4 py-2.5 font-medium max-md:hidden">{t('invites.col.created')}</th>
            <th className="px-4 py-2.5">
              <span className="sr-only">{t('invites.col.actions')}</span>
            </th>
          </tr>
        </thead>
        <tbody className="divide-y">
          {invites.map((inv) => {
            const status = inviteStatus(inv, now);
            const devices = inv.max_uses
              ? t('invites.pairedOf', { used: inv.uses, max: inv.max_uses })
              : t('invites.pairedUnlimited', { count: inv.uses });
            const expires = inv.expires_at
              ? formatRelative(inv.expires_at, lang, now)
              : t('invite.never');
            return (
              <tr key={inv.id} className="hover:bg-muted/70">
                {showFor ? (
                  <td className="px-4 py-3">
                    <Link
                      to="/people/user/$userId"
                      params={{ userId: String(inv.user_id) }}
                      className="flex items-center gap-2.5 font-semibold hover:underline"
                    >
                      <Monogram name={inv.username} size={28} />
                      <span className="truncate">{inv.username}</span>
                    </Link>
                  </td>
                ) : null}
                <td className="px-4 py-3">
                  <Badge variant={STATUS_BADGE[status]}>{t(`invites.status.${status}`)}</Badge>
                  <div className="mt-1 text-[12px] text-muted-foreground md:hidden">
                    {devices} · {expires}
                  </div>
                </td>
                <td className="px-4 py-3 text-right tabular-nums max-md:hidden">{devices}</td>
                <td className="px-4 py-3 text-muted-foreground max-md:hidden">{expires}</td>
                <td className="px-4 py-3 text-muted-foreground max-md:hidden">
                  {formatRelative(inv.created_at, lang, now)}
                </td>
                <td className="px-4 py-3">
                  <div className="flex justify-end gap-1">
                    {status !== 'usedUp' ? (
                      <Button
                        variant="outline"
                        size="sm"
                        onClick={() => void rotate(inv)}
                        aria-label={t('invites.rotateAria', { name: inv.username })}
                      >
                        <RefreshCw aria-hidden="true" />
                        <span className="max-sm:sr-only">{t('invites.rotate')}</span>
                      </Button>
                    ) : null}
                    <Button
                      variant="ghost"
                      size="sm"
                      className="text-destructive"
                      onClick={() => setRevoking(inv)}
                      aria-label={t('invites.revokeAria', { name: inv.username })}
                    >
                      {t('invites.revoke')}
                    </Button>
                  </div>
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
      <Dialog open={!!rotated} onOpenChange={(o) => !o && setRotated(undefined)}>
        {rotated ? <InviteResultContent invite={rotated} /> : null}
      </Dialog>
      <ConfirmDialog
        open={!!revoking}
        onOpenChange={(o) => !o && setRevoking(undefined)}
        icon={MailX}
        title={t('invites.revokeTitle', { name: revoking?.username ?? '' })}
        description={t('invites.revokeBody')}
        confirmLabel={t('invites.revoke')}
        onConfirm={async () => {
          if (!revoking) return;
          await api.revokeInvite(revoking.id);
          invalidatePeople(qc);
          toast.add({
            title: t('invites.toast.revoked', { name: revoking.username }),
            type: 'success',
          });
        }}
      />
    </div>
  );
}
