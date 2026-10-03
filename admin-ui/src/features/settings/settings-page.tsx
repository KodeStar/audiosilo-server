import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { Info, TriangleAlert } from 'lucide-react';
import { api } from '@/api/client';
import { keys, useSettings } from '@/api/hooks';
import type { AdminSettings } from '@/api/types';
import { Notice } from '@/components/notice';
import { Page } from '@/components/page';
import { PageHead } from '@/components/page-head';
import { QueryError } from '@/components/query-error';
import { Card, CardHeader } from '@/components/ui/card';
import { Switch } from '@/components/ui/switch';
import { toastError } from '@/lib/errors';
import { SettingRow } from '@/components/setting-row';
import { toast } from '@/lib/toast';

/**
 * Server > Settings. Today it holds the one setting the server can change at
 * runtime, the community metadata lookup; the rest of config.yaml moves here in
 * Phase 5a, each setting in exactly one place.
 */
export function SettingsPage() {
  const { t } = useTranslation();
  const settings = useSettings();
  return (
    <Page>
      <PageHead title={t('settings.title')} description={t('settings.description')} />
      <div className="flex max-w-[760px] flex-col gap-5">
        {settings.isError ? (
          <QueryError
            title={t('settings.error')}
            error={settings.error}
            onRetry={() => void settings.refetch()}
          />
        ) : !settings.data ? (
          <div
            className="skel h-[180px] rounded-xl"
            role="status"
            aria-label={t('common.loading')}
          />
        ) : (
          <MetadataCard settings={settings.data} />
        )}
        <Notice tone="info" icon={Info}>
          {t('settings.moreSoon')}
        </Notice>
      </div>
    </Page>
  );
}

function MetadataCard({ settings }: { settings: AdminSettings }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const m = settings.metadata;

  const toggle = async (enabled: boolean) => {
    const before = qc.getQueryData<AdminSettings>(keys.settings);
    qc.setQueryData<AdminSettings>(keys.settings, { metadata: { ...m, enabled } });
    try {
      const saved = await api.updateSettings({ metadata: { enabled } });
      qc.setQueryData(keys.settings, saved);
      // The `metadata` capability follows the switch.
      void qc.invalidateQueries({ queryKey: keys.server });
      toast.add({
        title: saved.metadata.enabled ? t('settings.metadata.on') : t('settings.metadata.off'),
        description: saved.metadata.enabled ? undefined : t('settings.metadata.offBody'),
        type: 'success',
      });
    } catch (err) {
      qc.setQueryData(keys.settings, before);
      toastError(t('settings.metadata.failed'), err);
    }
  };

  return (
    <Card aria-labelledby="metadata-title">
      <CardHeader
        titleId="metadata-title"
        title={t('settings.metadata.title')}
        description={t('settings.metadata.description')}
      />
      <div className="divide-y px-5">
        <SettingRow
          title={t('settings.metadata.toggle')}
          htmlFor="metadata-switch"
          description={t('settings.metadata.toggleBody')}
          descriptionId="metadata-switch-desc"
        >
          <Switch
            id="metadata-switch"
            checked={m.enabled && m.available}
            disabled={!m.available}
            onCheckedChange={(v) => void toggle(v)}
            aria-describedby="metadata-switch-desc"
          />
        </SettingRow>
        <SettingRow title={t('settings.metadata.address')}>
          <span className="font-mono text-[12.5px] text-muted-foreground [overflow-wrap:anywhere]">
            {m.base_url || t('settings.metadata.notConfigured')}
          </span>
        </SettingRow>
      </div>
      {!m.available ? (
        <Notice
          tone="warn"
          icon={TriangleAlert}
          title={t('settings.metadata.unavailable')}
          className="mx-5 mb-5"
        >
          {t('settings.metadata.unavailableBody')}
        </Notice>
      ) : null}
    </Card>
  );
}
