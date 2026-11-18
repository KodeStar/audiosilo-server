import { useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { Unlink } from 'lucide-react';
import { api, ApiError } from '@/api/client';
import { invalidatePeople, keys } from '@/api/hooks';
import type { AdminUser, OIDCIdentity } from '@/api/types';
import { ConfirmDialog } from '@/components/confirm-dialog';
import { FactList } from '@/components/fact-list';
import { Button } from '@/components/ui/button';
import { Card } from '@/components/ui/card';
import { formatRelative } from '@/lib/format';
import { toast } from '@/lib/toast';
import { identityProvider } from './pending-model';

/**
 * A person's linked single sign-ons (server#235), one card each: "Signed in
 * with <provider>", the email it carries, how and when it was linked, its last
 * sign-in, and Unlink. Unlinking a password-less admin's way into the console
 * is refused (409 admin_needs_password), said in the dialog. A 404 (unlinked
 * already, in another tab or by another admin) reads as done, and the person
 * is read again.
 */
export function IdentityCards({
  user,
  identities,
}: {
  user: AdminUser;
  identities: readonly OIDCIdentity[];
}) {
  return (
    <>
      {identities.map((i) => (
        <IdentityCard key={i.id} user={user} identity={i} provider={identityProvider(i)} />
      ))}
    </>
  );
}

function IdentityCard({
  user: u,
  identity: i,
  provider,
}: {
  user: AdminUser;
  identity: OIDCIdentity;
  provider: string;
}) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const qc = useQueryClient();
  const [unlinking, setUnlinking] = useState(false);
  const titleId = `identity-${i.id}-title`;
  const how =
    i.link_method === 'auto_email'
      ? t('user.identity.how.auto_email')
      : i.link_method === 'created'
        ? t('user.identity.how.created')
        : i.linked_by
          ? t('user.identity.how.adminBy', { name: i.linked_by.username })
          : t('user.identity.how.admin');
  return (
    <Card className="flex flex-col gap-3 p-5" aria-labelledby={titleId}>
      <h2 id={titleId} className="h3 [overflow-wrap:anywhere]">
        {t('user.identity.title', { provider })}
      </h2>
      <FactList
        rows={[
          [t('user.identity.email'), i.email || t('user.identity.noEmail')],
          [t('user.identity.linked'), `${how} · ${formatRelative(i.linked_at, lang)}`],
          [
            t('user.identity.lastSignIn'),
            i.last_sign_in_at ? formatRelative(i.last_sign_in_at, lang) : t('user.identity.never'),
          ],
        ]}
      />
      <div>
        <Button size="sm" variant="destructive-outline" onClick={() => setUnlinking(true)}>
          <Unlink aria-hidden="true" />
          {t('user.identity.unlink')}
        </Button>
      </div>
      <ConfirmDialog
        open={unlinking}
        onOpenChange={setUnlinking}
        icon={Unlink}
        title={t('user.identity.unlinkTitle', { provider, name: u.username })}
        description={t('user.identity.unlinkBody', { provider })}
        confirmLabel={t('user.identity.unlink')}
        onConfirm={async () => {
          try {
            await api.unlinkIdentity(u.id, i.id);
          } catch (err) {
            if (!(err instanceof ApiError && err.status === 404)) throw err;
          }
          invalidatePeople(qc);
          // The waiting sign-ins' Link offers this account again.
          void qc.invalidateQueries({ queryKey: keys.oidcPending });
          toast.add({ title: t('user.identity.unlinked', { provider }), type: 'success' });
        }}
      />
    </Card>
  );
}
