import { useState } from 'react';
import { Controller, useForm } from 'react-hook-form';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { BellRing } from 'lucide-react';
import { ApiError, api } from '@/api/client';
import { keys } from '@/api/hooks';
import type {
  NotifyTarget,
  NotifyTargetInput,
  NotifyTargetKind,
  ServerEventKind,
} from '@/api/types';
import { Checkbox } from '@/components/ui/checkbox';
import { Dialog, DialogBody, DialogContent, DialogFormFooter } from '@/components/ui/dialog';
import { Field, FormError } from '@/components/ui/field';
import { Input } from '@/components/ui/input';
import { RadioCards } from '@/components/ui/radio-cards';
import { describedBy } from '@/lib/a11y';
import { errorMessage } from '@/lib/errors';
import { EVENT_KINDS } from '@/lib/server-events';
import { toast } from '@/lib/toast';
import { DEFAULT_EVENTS, TARGET_FORM, TARGET_KINDS, toggleEvent } from './notify-model';

interface Values {
  kind: NotifyTargetKind;
  name: string;
  url: string;
  secret: string;
  clearSecret: boolean;
  events: ServerEventKind[];
}

type FieldName = 'kind' | 'name' | 'url' | 'secret' | 'events';

/**
 * Adds a destination, or (with `target`) changes one. Its address and secret
 * never come back from the server: when changing, an empty field keeps what is
 * saved.
 */
export function TargetDialog({
  target,
  open,
  onOpenChange,
}: {
  target?: NotifyTarget;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const { t } = useTranslation();
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        icon={BellRing}
        title={
          target ? t('notify.dialog.editTitle', { name: target.name }) : t('notify.dialog.addTitle')
        }
        description={target ? undefined : t('notify.dialog.addBody')}
      >
        {open ? <TargetForm target={target} onDone={() => onOpenChange(false)} /> : null}
      </DialogContent>
    </Dialog>
  );
}

function TargetForm({ target, onDone }: { target?: NotifyTarget; onDone: () => void }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const [formError, setFormError] = useState<string>();
  const form = useForm<Values>({
    defaultValues: {
      kind: target?.kind ?? 'webhook',
      name: target?.name ?? '',
      url: '',
      secret: '',
      clearSecret: false,
      events: target?.events ?? [...DEFAULT_EVENTS],
    },
  });
  const { errors, isSubmitting } = form.formState;
  const kind = form.watch('kind');
  const clearSecret = form.watch('clearSecret');
  const meta = TARGET_FORM[kind];

  const onSubmit = form.handleSubmit(async (v) => {
    setFormError(undefined);
    let missing = false;
    if (!v.name.trim()) {
      form.setError('name', { message: t('notify.dialog.nameRequired') });
      missing = true;
    }
    if (!target && !v.url.trim()) {
      form.setError('url', { message: t('notify.dialog.urlRequired') });
      missing = true;
    }
    if (missing) return;
    const input: NotifyTargetInput = { name: v.name.trim(), events: v.events };
    if (v.url.trim()) input.url = v.url.trim();
    if (meta.secret) {
      if (v.clearSecret) input.secret = '';
      else if (v.secret) input.secret = v.secret;
    }
    try {
      if (target) await api.updateNotifyTarget(target.id, input);
      else await api.createNotifyTarget({ ...input, kind: v.kind, enabled: true });
      await qc.invalidateQueries({ queryKey: keys.notifyTargets });
      toast.add({
        title: target
          ? t('notify.toast.saved', { name: input.name })
          : t('notify.toast.added', { name: input.name }),
        description: target ? undefined : t('notify.toast.addedBody'),
        type: 'success',
      });
      onDone();
    } catch (err) {
      const field =
        err instanceof ApiError && err.code === 'invalid_target' ? err.field : undefined;
      if (field && ['kind', 'name', 'url', 'secret', 'events'].includes(field)) {
        form.setError(field as FieldName, { message: errorMessage(err, t) });
      } else {
        setFormError(errorMessage(err, t));
      }
    }
  });

  const nameErr = errors.name?.message;
  const urlErr = errors.url?.message;
  const secretErr = errors.secret?.message;
  const eventsErr = errors.events?.message;
  return (
    <form onSubmit={(e) => void onSubmit(e)} noValidate className="contents">
      <DialogBody className="flex flex-col gap-4">
        {!target ? (
          <Controller
            control={form.control}
            name="kind"
            render={({ field }) => (
              <RadioCards
                label={t('notify.dialog.kind')}
                value={field.value}
                onValueChange={field.onChange}
                className="grid gap-2 sm:grid-cols-3"
                options={TARGET_KINDS.map((k) => ({
                  value: k,
                  title: t(`notify.kind.${k}`),
                  description: t(`notify.kind.${k}Body`),
                }))}
              />
            )}
          />
        ) : null}
        <Field htmlFor="target-name" label={t('notify.dialog.name')} error={nameErr}>
          <Input
            id="target-name"
            autoComplete="off"
            placeholder={t(`notify.dialog.namePlaceholder.${kind}`)}
            aria-invalid={nameErr ? true : undefined}
            aria-describedby={describedBy('target-name', !!nameErr, false)}
            {...form.register('name')}
          />
        </Field>
        <Field
          htmlFor="target-url"
          label={t(`notify.dialog.url.${kind}`)}
          error={urlErr}
          description={
            target
              ? t('notify.dialog.urlKeep', { address: target.address })
              : t(`notify.dialog.urlHint.${kind}`)
          }
        >
          <Input
            id="target-url"
            type="url"
            autoComplete="off"
            spellCheck={false}
            className="font-mono text-[12.5px]"
            placeholder={target ? target.address : meta.placeholder}
            aria-invalid={urlErr ? true : undefined}
            aria-describedby={describedBy('target-url', !!urlErr, true)}
            {...form.register('url')}
          />
        </Field>
        {meta.secret ? (
          <Field
            htmlFor="target-secret"
            label={t(`notify.dialog.secret.${meta.secret}`)}
            error={secretErr}
            description={
              target?.has_secret
                ? t('notify.dialog.secretKeep')
                : t(`notify.dialog.secretHint.${meta.secret}`)
            }
          >
            <Input
              id="target-secret"
              type="password"
              autoComplete="new-password"
              spellCheck={false}
              disabled={clearSecret}
              className="font-mono text-[12.5px]"
              aria-invalid={secretErr ? true : undefined}
              aria-describedby={describedBy('target-secret', !!secretErr, true)}
              {...form.register('secret')}
            />
            {target?.has_secret ? (
              <label className="flex items-center gap-2 text-[13px]">
                <Controller
                  control={form.control}
                  name="clearSecret"
                  render={({ field }) => (
                    <Checkbox checked={field.value} onCheckedChange={(c) => field.onChange(c)} />
                  )}
                />
                {t('notify.dialog.secretClear')}
              </label>
            ) : null}
          </Field>
        ) : null}
        <fieldset className="flex flex-col gap-2">
          <legend className="mb-1.5 text-[13px] font-semibold">{t('notify.dialog.events')}</legend>
          <Controller
            control={form.control}
            name="events"
            render={({ field }) => (
              <div className="grid gap-x-4 gap-y-2 sm:grid-cols-2">
                {EVENT_KINDS.map((k) => (
                  <label key={k} className="flex items-start gap-2 text-[13px]">
                    <Checkbox
                      className="mt-0.5"
                      checked={field.value.includes(k)}
                      onCheckedChange={(on) =>
                        field.onChange(toggleEvent(field.value, k, Boolean(on), EVENT_KINDS))
                      }
                    />
                    {t(`notify.event.${k}`)}
                  </label>
                ))}
              </div>
            )}
          />
          {eventsErr ? (
            <p role="alert" className="text-[12.5px] font-medium text-destructive">
              {eventsErr}
            </p>
          ) : null}
        </fieldset>
        <FormError>{formError}</FormError>
      </DialogBody>
      <DialogFormFooter
        busy={isSubmitting}
        submitLabel={target ? t('notify.dialog.save') : t('notify.dialog.add')}
      />
    </form>
  );
}
