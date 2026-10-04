import { useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import {
  BellRing,
  Ellipsis,
  LoaderCircle,
  MessageSquare,
  Pencil,
  Plus,
  Send,
  ShieldCheck,
  Trash2,
  Webhook,
  type LucideIcon,
} from 'lucide-react';
import { api } from '@/api/client';
import { keys, useNotifyTargets } from '@/api/hooks';
import type {
  NotifyTarget,
  NotifyTargetKind,
  NotifyTargetsEnvelope,
  ServerEventKind,
} from '@/api/types';
import { ConfirmDialog } from '@/components/confirm-dialog';
import { Notice } from '@/components/notice';
import { QueryError } from '@/components/query-error';
import { StatusText } from '@/components/status-text';
import { Badge } from '@/components/ui/badge';
import { Button, buttonVariants } from '@/components/ui/button';
import { Card, CardHeader } from '@/components/ui/card';
import { Checkbox } from '@/components/ui/checkbox';
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu';
import { Switch } from '@/components/ui/switch';
import { toastError } from '@/lib/errors';
import { formatRelative } from '@/lib/format';
import { toast } from '@/lib/toast';
import { CardEmpty } from './card-empty';
import { deliveryLook, knownEvents, reasonText, toggleEvent, withTarget } from './notify-model';
import { TargetDialog } from './target-dialog';

const KIND_ICONS: Record<NotifyTargetKind, LucideIcon> = {
  webhook: Webhook,
  ntfy: BellRing,
  discord: MessageSquare,
};

/**
 * Settings > Notifications: where the server sends what happens (a webhook, an
 * ntfy topic, a Discord channel), which events go where, and "Send test". The
 * same events always show in the console's bell.
 */
export function NotificationsTopic() {
  const { t } = useTranslation();
  const targets = useNotifyTargets();
  const [adding, setAdding] = useState(false);
  if (targets.isError)
    return (
      <QueryError
        title={t('notify.error')}
        error={targets.error}
        onRetry={() => void targets.refetch()}
      />
    );
  if (!targets.data)
    return (
      <div className="skel h-[220px] rounded-xl" role="status" aria-label={t('common.loading')} />
    );
  const env = targets.data;
  return (
    <>
      <Card aria-labelledby="targets-title">
        <CardHeader
          titleId="targets-title"
          title={t('notify.targetsCard')}
          description={t('notify.targetsBody')}
          action={
            env.targets.length ? (
              <Button size="sm" onClick={() => setAdding(true)}>
                <Plus aria-hidden="true" />
                {t('notify.add')}
              </Button>
            ) : undefined
          }
        />
        {env.targets.length === 0 ? (
          <CardEmpty
            icon={BellRing}
            title={t('notify.empty.title')}
            body={t('notify.empty.body')}
            action={
              <Button size="sm" onClick={() => setAdding(true)}>
                <Plus aria-hidden="true" />
                {t('notify.add')}
              </Button>
            }
          />
        ) : (
          <ul className="flex flex-col divide-y border-t" aria-label={t('notify.targetsCard')}>
            {env.targets.map((target) => (
              <TargetRow key={target.id} target={target} events={knownEvents(env)} />
            ))}
          </ul>
        )}
      </Card>
      {env.targets.length ? <EventMatrix env={env} /> : null}
      <Notice tone="info" icon={ShieldCheck}>
        {t('notify.privacy')}
      </Notice>
      <TargetDialog open={adding} onOpenChange={setAdding} events={knownEvents(env)} />
    </>
  );
}

/** Saves a change to one destination, shown at once; restores the list on a failure. */
function useSaveTarget() {
  const qc = useQueryClient();
  return async (
    id: number,
    change: Parameters<typeof api.updateNotifyTarget>[1],
    failed: string,
  ) => {
    const before = qc.getQueryData<NotifyTargetsEnvelope>(keys.notifyTargets);
    if (before) {
      qc.setQueryData<NotifyTargetsEnvelope>(keys.notifyTargets, {
        ...before,
        targets: before.targets.map((x) =>
          x.id === id ? ({ ...x, ...change } as NotifyTarget) : x,
        ),
      });
    }
    try {
      const saved = await api.updateNotifyTarget(id, change);
      qc.setQueryData<NotifyTargetsEnvelope>(
        keys.notifyTargets,
        (env) => env && withTarget(env, saved),
      );
    } catch (err) {
      if (before) qc.setQueryData(keys.notifyTargets, before);
      toastError(failed, err);
      void qc.invalidateQueries({ queryKey: keys.notifyTargets });
    }
  };
}

function TargetRow({ target, events }: { target: NotifyTarget; events: ServerEventKind[] }) {
  const { t, i18n } = useTranslation();
  const lang = i18n.resolvedLanguage ?? 'en';
  const qc = useQueryClient();
  const save = useSaveTarget();
  const [testing, setTesting] = useState(false);
  const [dialog, setDialog] = useState<'edit' | 'delete' | null>(null);
  const Icon = KIND_ICONS[target.kind];
  const look = deliveryLook(target);

  const test = async () => {
    setTesting(true);
    try {
      const r = await api.testNotifyTarget(target.id);
      if (r.ok) {
        toast.add({
          title: t('notify.test.sent', { name: target.name }),
          description: t('notify.test.sentBody'),
          type: 'success',
        });
      } else {
        const reason = reasonText(r.error);
        toast.add({
          title: t('notify.test.failed', { name: target.name }),
          description: t(reason.key, reason.values),
        });
      }
      qc.setQueryData<NotifyTargetsEnvelope>(
        keys.notifyTargets,
        (env) => env && withTarget(env, r.target),
      );
    } catch (err) {
      toastError(t('notify.test.failed', { name: target.name }), err);
    } finally {
      setTesting(false);
    }
  };

  return (
    <li className="flex flex-wrap items-center gap-x-3 gap-y-2 px-5 py-3.5">
      <span
        className="grid size-[34px] shrink-0 place-items-center rounded-[10px] bg-muted text-muted-foreground"
        aria-hidden="true"
      >
        <Icon className="size-4" />
      </span>
      <div className="flex min-w-0 flex-1 basis-52 flex-col gap-0.5">
        <span className="flex flex-wrap items-center gap-2">
          <b className="font-semibold [overflow-wrap:anywhere]">{target.name}</b>
          <Badge variant="outline">{t(`notify.kind.${target.kind}`)}</Badge>
        </span>
        <span className="font-mono text-[11.5px] text-muted-foreground [overflow-wrap:anywhere]">
          {target.address}
        </span>
        <StatusText tone={look.tone} className="text-[12px] text-muted-foreground">
          {t(look.key, { when: target.last_at ? formatRelative(target.last_at, lang) : '' })}
          {look.reason ? `: ${t(look.reason.key, look.reason.values)}` : ''}
        </StatusText>
      </div>
      <div className="flex items-center gap-1.5">
        <Button variant="ghost" size="sm" disabled={testing} onClick={() => void test()}>
          {testing ? (
            <LoaderCircle className="animate-spin" aria-hidden="true" />
          ) : (
            <Send aria-hidden="true" />
          )}
          {t('notify.test.action')}
        </Button>
        <Switch
          checked={target.enabled}
          aria-label={t('notify.enabledAria', { name: target.name })}
          onCheckedChange={(on) =>
            void save(
              target.id,
              { enabled: on },
              t('notify.toast.saveFailed', { name: target.name }),
            )
          }
        />
        <DropdownMenu>
          <DropdownMenuTrigger
            className={buttonVariants({ variant: 'ghost', size: 'icon-sm' })}
            aria-label={t('notify.actions', { name: target.name })}
          >
            <Ellipsis className="size-4" aria-hidden="true" />
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end">
            <DropdownMenuItem onClick={() => setDialog('edit')}>
              <Pencil aria-hidden="true" />
              {t('notify.edit')}
            </DropdownMenuItem>
            <DropdownMenuSeparator />
            <DropdownMenuItem variant="destructive" onClick={() => setDialog('delete')}>
              <Trash2 aria-hidden="true" />
              {t('notify.delete.action')}
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </div>
      <TargetDialog
        target={target}
        events={events}
        open={dialog === 'edit'}
        onOpenChange={(o) => setDialog(o ? 'edit' : null)}
      />
      <ConfirmDialog
        open={dialog === 'delete'}
        onOpenChange={(o) => setDialog(o ? 'delete' : null)}
        title={t('notify.delete.title', { name: target.name })}
        description={t('notify.delete.body')}
        icon={Trash2}
        confirmLabel={t('notify.delete.confirm')}
        onConfirm={async () => {
          await api.deleteNotifyTarget(target.id);
          toast.add({ title: t('notify.delete.done', { name: target.name }), type: 'success' });
          await qc.invalidateQueries({ queryKey: keys.notifyTargets });
        }}
      />
    </li>
  );
}

/** Which events go to which destination: a checkbox per pair, saved as it is ticked. */
function EventMatrix({ env }: { env: NotifyTargetsEnvelope }) {
  const { t } = useTranslation();
  const save = useSaveTarget();
  const kinds = knownEvents(env);
  const flip = (target: NotifyTarget, kind: ServerEventKind, on: boolean) =>
    void save(
      target.id,
      { events: toggleEvent(target.events, kind, on, kinds) },
      t('notify.toast.saveFailed', { name: target.name }),
    );
  return (
    <Card aria-labelledby="events-title">
      <CardHeader
        titleId="events-title"
        title={t('notify.eventsCard')}
        description={t('notify.eventsBody')}
      />
      <div className="overflow-x-auto border-t">
        <table className="w-full min-w-[480px] text-[13px]">
          <thead>
            <tr className="text-left text-[12px] text-muted-foreground">
              <th scope="col" className="px-5 py-2.5 font-semibold">
                {t('notify.event.column')}
              </th>
              {env.targets.map((target) => (
                <th key={target.id} scope="col" className="px-3 py-2.5 text-center font-semibold">
                  <span
                    className="inline-block max-w-[140px] truncate align-bottom"
                    title={target.name}
                  >
                    {target.name}
                  </span>
                </th>
              ))}
            </tr>
          </thead>
          <tbody className="divide-y">
            {kinds.map((kind) => (
              <tr key={kind}>
                <th scope="row" className="px-5 py-3 text-left font-normal">
                  <b className="font-semibold">{t(`notify.event.${kind}`)}</b>
                  <span className="block text-[12px] text-muted-foreground">
                    {t(`notify.event.${kind}Body`)}
                  </span>
                </th>
                {env.targets.map((target) => (
                  <td key={target.id} className="px-3 py-3 text-center">
                    <Checkbox
                      className="mx-auto"
                      checked={target.events.includes(kind)}
                      onCheckedChange={(on) => flip(target, kind, Boolean(on))}
                      aria-label={t('notify.event.cellAria', {
                        event: t(`notify.event.${kind}`),
                        name: target.name,
                      })}
                    />
                  </td>
                ))}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </Card>
  );
}
