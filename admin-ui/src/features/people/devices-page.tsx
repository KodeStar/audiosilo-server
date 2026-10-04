import { useState } from 'react';
import { Link } from '@tanstack/react-router';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { LogOut, Smartphone } from 'lucide-react';
import { api } from '@/api/client';
import { invalidateDevices, useDevices } from '@/api/hooks';
import type { Device } from '@/api/types';
import { ConfirmDialog } from '@/components/confirm-dialog';
import { EmptyState } from '@/components/empty-state';
import { Monogram } from '@/components/monogram';
import { Page } from '@/components/page';
import { PageHead } from '@/components/page-head';
import { QueryError } from '@/components/query-error';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { ClientIcon } from '@/features/activity/client-label';
import { useClientName } from '@/features/activity/use-client-name';
import { formatDateTime, formatRelative } from '@/lib/format';
import { toast } from '@/lib/toast';

/** People > Devices: every paired phone, browser and API key, each of which can be signed out. */
export function DevicesPage() {
  const { t } = useTranslation();
  const devices = useDevices();
  return (
    <Page>
      <PageHead title={t('devices.title')} description={t('devices.description')} />
      {devices.isError ? (
        <QueryError
          title={t('devices.error')}
          error={devices.error}
          onRetry={() => void devices.refetch()}
        />
      ) : !devices.data ? (
        <div className="flex flex-col gap-2" role="status" aria-label={t('common.loading')}>
          {[0, 1, 2].map((i) => (
            <div key={i} className="skel h-14" />
          ))}
        </div>
      ) : devices.data.length === 0 ? (
        <EmptyState
          icon={Smartphone}
          title={t('devices.empty.title')}
          body={t('devices.empty.body')}
        />
      ) : (
        <DeviceList devices={devices.data} showPerson />
      )}
    </Page>
  );
}

/**
 * Devices as rows: what it is, whose, which app, when and where it was last seen,
 * and Sign out (confirmed; the console's own device can't be signed out here).
 */
export function DeviceList({ devices, showPerson }: { devices: Device[]; showPerson: boolean }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const clientName = useClientName();
  const [revoking, setRevoking] = useState<Device>();
  return (
    <div className="overflow-hidden rounded-xl border bg-card">
      <ul className="divide-y">
        {devices.map((d) => {
          const key = d.kind === 'api';
          const name = d.name || t('live.unnamed');
          const app = key ? t('devices.apiKey') : clientName(d.client);
          const seen = d.last_seen
            ? t('devices.seen', { time: formatRelative(d.last_seen, lang) })
            : t('devices.neverSeen');
          return (
            <li key={d.id} className="flex flex-wrap items-center gap-x-3.5 gap-y-2 px-[18px] py-3">
              <ClientIcon client={d.client} apiKey={key} />
              <div className="flex min-w-0 flex-1 basis-56 flex-col">
                <span className="flex min-w-0 flex-wrap items-center gap-2">
                  <b className="truncate font-semibold">{name}</b>
                  {d.current ? <Badge variant="brand">{t('devices.current')}</Badge> : null}
                </span>
                <span className="text-[12.5px] text-muted-foreground [overflow-wrap:anywhere]">
                  {[app, seen, d.last_ip ? t('devices.from', { ip: d.last_ip }) : '']
                    .filter(Boolean)
                    .join(' · ')}
                </span>
                <span
                  className="text-[12px] text-subtle-foreground"
                  title={formatDateTime(d.created_at, lang)}
                >
                  {key
                    ? t('devices.created', { time: formatRelative(d.created_at, lang) })
                    : t('devices.paired', { time: formatRelative(d.created_at, lang) })}
                </span>
              </div>
              {showPerson ? (
                <Link
                  to="/people/user/$userId"
                  params={{ userId: String(d.user_id) }}
                  className="flex min-w-0 items-center gap-2 text-[13px] font-semibold hover:underline"
                >
                  <Monogram name={d.username} size={24} />
                  <span className="truncate">{d.username}</span>
                </Link>
              ) : null}
              <Button
                variant="outline"
                size="sm"
                disabled={d.current}
                title={d.current ? t('devices.currentHint') : undefined}
                onClick={() => setRevoking(d)}
                aria-label={t('devices.signOutAria', {
                  name,
                  person: d.username,
                })}
              >
                <LogOut aria-hidden="true" />
                {key ? t('devices.revoke') : t('devices.signOut')}
              </Button>
            </li>
          );
        })}
      </ul>
      {revoking ? <RevokeDialog device={revoking} onClose={() => setRevoking(undefined)} /> : null}
    </div>
  );
}

/** Confirms signing a device out (or revoking an API key), naming it and its person. */
function RevokeDialog({ device: d, onClose }: { device: Device; onClose: () => void }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const name = d.name || t('live.unnamed');
  const key = d.kind === 'api';
  return (
    <ConfirmDialog
      open
      onOpenChange={(open) => !open && onClose()}
      icon={LogOut}
      title={
        key
          ? t('devices.confirm.keyTitle', { name, person: d.username })
          : t('devices.confirm.title', { name, person: d.username })
      }
      description={
        key ? t('devices.confirm.keyBody') : t('devices.confirm.body', { person: d.username })
      }
      confirmLabel={key ? t('devices.revoke') : t('devices.signOut')}
      onConfirm={async () => {
        await api.revokeDevice(d.id);
        invalidateDevices(qc);
        toast.add({ title: t('devices.done', { name }), type: 'success' });
      }}
    />
  );
}
