import { useEffect, useRef, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import {
  Archive,
  ArchiveRestore,
  CircleCheck,
  Database,
  Download,
  LoaderCircle,
  RotateCcw,
  Trash2,
  TriangleAlert,
} from 'lucide-react';
import { api, downloadBackup } from '@/api/client';
import { keys, useBackups } from '@/api/hooks';
import type { AdminSettings, Backup, BackupsEnvelope } from '@/api/types';
import { ConfirmDialog } from '@/components/confirm-dialog';
import { Notice } from '@/components/notice';
import { QueryError } from '@/components/query-error';
import { SettingRow } from '@/components/setting-row';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Card, CardHeader } from '@/components/ui/card';
import { toastError } from '@/lib/errors';
import { formatBytes, formatDateTime, formatNumber } from '@/lib/format';
import { toast } from '@/lib/toast';
import { backupFailureKey } from '@/lib/server-events';
import { CardEmpty } from './card-empty';
import { describeSchedule, restoreFailureKey, totalSize } from './backups-model';
import { SettingBadges, SettingsForm } from './settings-form';

/** How long the outcome of the last restore stays on this page. */
const RESTORE_NEWS_MS = 14 * 86_400_000;

/**
 * Settings > Backups: the schedule, the backups in the folder (download,
 * restore, delete), "Back up now", and any restore waiting for a restart.
 */
export function BackupsTopic({ settings }: { settings: AdminSettings }) {
  const { t } = useTranslation();
  const backups = useBackups();
  return (
    <>
      <Notice tone="info" icon={Database} title={t('backups.why.title')}>
        {t('backups.why.body')}
      </Notice>
      {backups.data ? <RestoreNotices env={backups.data} /> : null}
      <SettingsForm
        settings={settings}
        section="backups"
        title={t('backups.scheduleCard')}
        fields={[
          { name: 'schedule', kind: 'schedule' },
          { name: 'keep', kind: 'number', placeholder: '7' },
        ]}
      >
        <SettingRow
          title={t('settings.backups.dir')}
          badges={<SettingBadges settings={settings} id="backups.dir" />}
          description={t('settings.backups.dirBody')}
        >
          <span className="font-mono text-[12.5px] [overflow-wrap:anywhere]">
            {backups.data?.status.dir ?? settings.backups.dir}
          </span>
        </SettingRow>
      </SettingsForm>
      {backups.isError ? (
        <QueryError
          title={t('backups.error')}
          error={backups.error}
          onRetry={() => void backups.refetch()}
        />
      ) : !backups.data ? (
        <div className="skel h-[200px] rounded-xl" role="status" aria-label={t('common.loading')} />
      ) : (
        <BackupList env={backups.data} schedule={settings.backups.schedule} />
      )}
    </>
  );
}

/** A restore waiting for a restart, and how the last one went (for a fortnight). */
function RestoreNotices({ env }: { env: BackupsEnvelope }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const qc = useQueryClient();
  const { pending, last } = env.restore;
  const [cancelling, setCancelling] = useState(false);
  const cancel = async () => {
    setCancelling(true);
    try {
      await api.cancelRestore();
      toast.add({ title: t('backups.restore.cancelled'), type: 'success' });
      await qc.invalidateQueries({ queryKey: keys.backups });
    } catch (err) {
      toastError(t('backups.restore.cancelFailed'), err);
    } finally {
      setCancelling(false);
    }
  };
  const recent = last && Date.now() - Date.parse(last.applied_at) < RESTORE_NEWS_MS;
  return (
    <>
      {pending ? (
        <Notice
          tone="warn"
          icon={RotateCcw}
          role="status"
          title={t('backups.restore.pendingTitle')}
          actions={
            <Button variant="outline" size="sm" disabled={cancelling} onClick={() => void cancel()}>
              {t('backups.restore.cancel')}
            </Button>
          }
        >
          {t('backups.restore.pendingBody', {
            name: pending.name,
            who: pending.requested_by,
            when: formatDateTime(pending.requested_at, lang),
          })}
        </Notice>
      ) : null}
      {recent && last ? (
        last.ok ? (
          <Notice
            tone="safe"
            icon={CircleCheck}
            title={t('backups.restore.doneTitle', {
              name: last.name,
              when: formatDateTime(last.applied_at, lang),
            })}
          >
            {last.safety_copy
              ? t('backups.restore.doneBody', { copy: last.safety_copy })
              : t('backups.restore.doneNoCopy')}
          </Notice>
        ) : (
          <Notice
            tone="bad"
            icon={TriangleAlert}
            title={t('backups.restore.failedTitle', { name: last.name })}
          >
            {t(restoreFailureKey(last.error))}
          </Notice>
        )
      ) : null}
    </>
  );
}

/** The backups in the folder, newest first, with "Back up now". */
function BackupList({ env, schedule }: { env: BackupsEnvelope; schedule: string }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const qc = useQueryClient();
  const { status, backups } = env;
  const [starting, setStarting] = useState(false);
  const [restoring, setRestoring] = useState<Backup | null>(null);
  const [deleting, setDeleting] = useState<Backup | null>(null);
  const running = status.running || starting;

  // A backup this page started tells how it went when it's done.
  const watching = useRef(false);
  useEffect(() => {
    if (!watching.current || status.running) return;
    watching.current = false;
    if (status.last?.ok)
      toast.add({
        title: t('backups.made'),
        description: status.last.name,
        type: 'success',
      });
    else
      toast.add({
        title: t('backups.failedToast'),
        description: t(backupFailureKey(status.last?.error)),
      });
  }, [status.running, status.last, t]);

  const backUpNow = async () => {
    setStarting(true);
    try {
      const next = await api.createBackup();
      watching.current = true;
      qc.setQueryData(keys.backups, next);
    } catch (err) {
      toastError(t('backups.startFailed'), err);
    } finally {
      setStarting(false);
    }
  };
  const save = async (b: Backup) => {
    try {
      await downloadBackup(b.name);
    } catch (err) {
      toastError(t('backups.downloadFailed'), err);
    }
  };
  const schedText = describeSchedule(schedule);

  return (
    <Card aria-labelledby="backups-title">
      <CardHeader
        titleId="backups-title"
        title={t('backups.listCard')}
        description={
          backups.length
            ? t('backups.summary', {
                count: backups.length,
                n: formatNumber(backups.length, lang),
                size: formatBytes(totalSize(backups), lang),
              })
            : undefined
        }
        action={
          <Button size="sm" disabled={running} onClick={() => void backUpNow()}>
            {running ? (
              <LoaderCircle className="animate-spin" aria-hidden="true" />
            ) : (
              <Archive aria-hidden="true" />
            )}
            {running ? t('backups.running') : t('backups.now')}
          </Button>
        }
      />
      {status.last && !status.last.ok ? (
        <Notice
          tone="bad"
          icon={TriangleAlert}
          className="mx-5 mb-4"
          title={t('backups.lastFailed', { when: formatDateTime(status.last.at, lang) })}
        >
          {t(backupFailureKey(status.last.error))}
        </Notice>
      ) : null}
      {backups.length === 0 ? (
        <CardEmpty
          icon={Archive}
          title={t('backups.empty.title')}
          body={
            status.next
              ? t('backups.empty.next', { when: formatDateTime(status.next, lang) })
              : t('backups.empty.off')
          }
        />
      ) : (
        <ul className="flex flex-col divide-y border-t" aria-label={t('backups.listCard')}>
          {backups.map((b) => (
            <li key={b.name} className="flex flex-wrap items-center gap-x-3 gap-y-2 px-5 py-3">
              <Archive className="size-4 shrink-0 text-subtle-foreground" aria-hidden="true" />
              <div className="flex min-w-0 flex-1 basis-48 flex-col gap-0.5">
                <span className="flex flex-wrap items-center gap-2">
                  <b className="font-semibold">{formatDateTime(b.created_at, lang)}</b>
                  <Badge variant={b.kind === 'before-restore' ? 'warning' : 'secondary'}>
                    {t(`backups.kind.${b.kind}`)}
                  </Badge>
                </span>
                <span className="font-mono text-[11.5px] text-muted-foreground [overflow-wrap:anywhere]">
                  {b.name} · {formatBytes(b.size, lang)}
                </span>
              </div>
              <div className="flex items-center gap-1.5">
                <Button variant="ghost" size="sm" onClick={() => void save(b)}>
                  <Download aria-hidden="true" />
                  {t('backups.download')}
                </Button>
                <Button variant="outline" size="sm" onClick={() => setRestoring(b)}>
                  {t('backups.restore.action')}
                </Button>
                <Button
                  variant="ghost"
                  size="icon-sm"
                  className="text-destructive hover:bg-destructive-soft"
                  aria-label={t('backups.deleteAria', { name: b.name })}
                  onClick={() => setDeleting(b)}
                >
                  <Trash2 aria-hidden="true" />
                </Button>
              </div>
            </li>
          ))}
        </ul>
      )}
      <p className="border-t px-5 py-3.5 text-[12.5px] text-muted-foreground">
        {t(schedText.key, schedText.values)}
        {status.next
          ? ` ${t('backups.nextAt', { when: formatDateTime(status.next, lang) })}`
          : ''}{' '}
        {t('backups.keepNote')}
      </p>

      <ConfirmDialog
        open={restoring !== null}
        onOpenChange={(o) => !o && setRestoring(null)}
        title={t('backups.restore.title')}
        description={
          restoring
            ? t('backups.restore.body', { when: formatDateTime(restoring.created_at, lang) })
            : undefined
        }
        icon={ArchiveRestore}
        confirmLabel={t('backups.restore.confirm')}
        typeToConfirm={t('backups.restore.word')}
        onConfirm={async () => {
          if (!restoring) return;
          qc.setQueryData(keys.backups, await api.restoreBackup(restoring.name));
          toast.add({
            title: t('backups.restore.scheduled'),
            description: t('backups.restore.scheduledBody'),
          });
        }}
      >
        <ul className="ml-5 flex list-disc flex-col gap-1 text-[13px] text-muted-foreground">
          <li>{t('backups.restore.point.copy')}</li>
          <li>{t('backups.restore.point.devices')}</li>
          <li>{t('backups.restore.point.revoked')}</li>
          <li>{t('backups.restore.point.config')}</li>
        </ul>
      </ConfirmDialog>
      <ConfirmDialog
        open={deleting !== null}
        onOpenChange={(o) => !o && setDeleting(null)}
        title={t('backups.delete.title')}
        description={
          deleting
            ? t('backups.delete.body', { when: formatDateTime(deleting.created_at, lang) })
            : undefined
        }
        icon={Trash2}
        confirmLabel={t('backups.delete.confirm')}
        onConfirm={async () => {
          if (!deleting) return;
          await api.deleteBackup(deleting.name);
          toast.add({ title: t('backups.delete.done'), type: 'success' });
          await qc.invalidateQueries({ queryKey: keys.backups });
        }}
      />
    </Card>
  );
}
