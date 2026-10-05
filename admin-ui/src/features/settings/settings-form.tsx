import { useId, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { Lock, RotateCcw, TriangleAlert } from 'lucide-react';
import { ApiError } from '@/api/client';
import { keys } from '@/api/hooks';
import type { AdminSettings, SettingsPatch, SettingsSection } from '@/api/types';
import { ConfirmDialog } from '@/components/confirm-dialog';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Card, CardHeader } from '@/components/ui/card';
import { FormError } from '@/components/ui/field';
import { Input } from '@/components/ui/input';
import { NativeSelect } from '@/components/ui/native-select';
import { RadioCards, type RadioCardOption } from '@/components/ui/radio-cards';
import { Switch } from '@/components/ui/switch';
import { Textarea } from '@/components/ui/textarea';
import { errorMessage, toastError } from '@/lib/errors';
import { toast } from '@/lib/toast';
import { ScheduleInput } from './schedule-input';
import { useSaveSettings } from './use-save-settings';
import { describedBy } from '@/lib/a11y';
import { cn } from '@/lib/utils';
import {
  listToText,
  lockOf,
  needsRestart,
  patchIds,
  sectionPatch,
  textToList,
  type SettingId,
} from './settings-model';

/** "Set by AUDIOSILO_X" / "Managed by the desktop app", and "Restart to apply". */
export function SettingBadges({ settings, id }: { settings: AdminSettings; id: SettingId }) {
  const { t } = useTranslation();
  const lock = lockOf(settings, id);
  const pending = settings.restart_pending.includes(id);
  return (
    <>
      {lock ? (
        <Badge variant="outline" title={t('settings.locked.hint')}>
          <Lock aria-hidden="true" />
          {lock === 'launcher'
            ? t('settings.locked.launcher')
            : t('settings.locked.env', { name: lock })}
        </Badge>
      ) : null}
      {pending ? (
        <Badge variant="warning">
          <RotateCcw aria-hidden="true" />
          {t('settings.restart.pending')}
        </Badge>
      ) : needsRestart(settings, id) && !lock ? (
        <Badge variant="secondary">{t('settings.restart.needed')}</Badge>
      ) : null}
    </>
  );
}

/** A form field of a settings card. Labels are `settings.<section>.<name>` (+ `Body`). */
export type FieldSpec =
  | { name: string; kind: 'text' | 'number'; placeholder?: string; mono?: boolean }
  | { name: string; kind: 'list'; placeholder?: string }
  | { name: string; kind: 'switch' }
  /** A backup schedule: how often, which day, what time (backups-model). */
  | { name: string; kind: 'schedule' }
  | { name: string; kind: 'select'; options: { value: string; label: string }[] }
  | { name: string; kind: 'radio'; options: RadioCardOption<string>[] };

/** The draft of one field: lists are edited as text (one entry per line). */
type DraftValue = string | boolean;

function toDraft(spec: FieldSpec, value: unknown): DraftValue {
  if (spec.kind === 'list') return listToText((value as string[]) ?? []);
  if (spec.kind === 'switch') return Boolean(value);
  if (spec.kind === 'number') return value === null || value === undefined ? '' : String(value);
  return (value as string) ?? '';
}

function toWire(spec: FieldSpec, value: DraftValue): unknown {
  if (spec.kind === 'list') return textToList(value as string);
  if (spec.kind === 'number') {
    const s = (value as string).trim();
    if (s === '') return null;
    // Not a number: send the text, so the server refuses it on the field
    // (Number() would give NaN, which JSON sends as null: "use the default").
    const n = Number(s);
    return Number.isFinite(n) ? n : s;
  }
  return value;
}

/**
 * One card of settings that saves together: its fields as a draft, "Reset" and
 * "Save changes" in the footer, only the changed fields sent. A refused field
 * shows the server's reason under it; nothing is saved then (the server takes
 * all or nothing). `confirmRestart` asks first when a change waits for a restart
 * and could keep the server from starting (listen address, HTTPS).
 */
export function SettingsForm<S extends SettingsSection>({
  settings,
  section,
  title,
  description,
  fields,
  visible,
  confirmRestart,
  children,
}: {
  settings: AdminSettings;
  section: S;
  title: string;
  description?: string;
  fields: FieldSpec[];
  /** Hides a field for the current draft (TLS hosts outside Let's Encrypt). */
  visible?: (name: string, draft: Record<string, DraftValue>) => boolean;
  confirmRestart?: boolean;
  /** Read-only rows after the fields (a certificate's status). */
  children?: React.ReactNode;
}) {
  const { t } = useTranslation();
  const formId = useId();
  const save = useSaveSettings();
  const saved = settings[section] as Record<string, unknown>;
  const initial = () =>
    Object.fromEntries(fields.map((f) => [f.name, toDraft(f, saved[f.name])])) as Record<
      string,
      DraftValue
    >;
  const [draft, setDraft] = useState(initial);
  const [savedFor, setSavedFor] = useState(saved);
  // A refusal: on the field it names (with the server's reason), or for the form.
  const [errors, setErrors] = useState<{ field?: string; fieldText?: string; form?: string }>({});
  const [busy, setBusy] = useState(false);
  const [confirming, setConfirming] = useState<SettingsPatch | null>(null);

  // A save (this card's or another's on the same section) or a refetch replaced
  // the saved values: fields whose saved value changed take it; edits to the
  // others stay.
  if (savedFor !== saved) {
    const was = savedFor;
    setSavedFor(saved);
    setDraft((d) => {
      const next = { ...d };
      for (const f of fields) {
        if (toDraft(f, was[f.name]) !== toDraft(f, saved[f.name])) {
          next[f.name] = toDraft(f, saved[f.name]);
        }
      }
      return next;
    });
  }

  // Every shown field set elsewhere (the environment, the desktop app): nothing to save here.
  const shown = fields.filter((f) => !visible || visible(f.name, draft));
  const editable = shown.some((f) => !lockOf(settings, `${section}.${f.name}` as SettingId));
  // Only the shown fields: an edit to one hidden since (TLS hosts after leaving
  // Let's Encrypt) isn't saved behind the admin's back.
  const wire = Object.fromEntries(shown.map((f) => [f.name, toWire(f, draft[f.name])]));
  const patch = sectionPatch(section, settings[section], wire as Partial<AdminSettings[S]>);

  const commit = async (p: SettingsPatch) => {
    setBusy(true);
    setErrors({});
    try {
      const next = await save(p);
      const waits = patchIds(p).some((id) => next.restart_pending.includes(id));
      toast.add({
        title: t('settings.saved', { section: title }),
        description: waits ? t('settings.savedRestart') : t('settings.savedNow'),
        type: 'success',
      });
    } catch (err) {
      if (err instanceof ApiError && err.field?.startsWith(`${section}.`)) {
        setErrors({ field: err.field.slice(section.length + 1), fieldText: errorMessage(err, t) });
      } else {
        setErrors({ form: errorMessage(err, t) });
      }
      throw err;
    } finally {
      setBusy(false);
    }
  };
  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    if (!patch) return;
    const risky = patchIds(patch).some((id) => needsRestart(settings, id));
    if (confirmRestart && risky) {
      setConfirming(patch);
      return;
    }
    commit(patch).catch(() => {});
  };

  return (
    <Card aria-labelledby={`${formId}-title`}>
      <CardHeader titleId={`${formId}-title`} title={title} description={description} />
      <form onSubmit={submit} noValidate>
        <div className="divide-y px-5">
          {shown.map((f) => (
            <FieldRow
              key={f.name}
              id={`${formId}-${f.name}`}
              spec={f}
              settingId={`${section}.${f.name}` as SettingId}
              settings={settings}
              value={draft[f.name]}
              onChange={(v) => setDraft((d) => ({ ...d, [f.name]: v }))}
              error={errors.field === f.name ? errors.fieldText : undefined}
            />
          ))}
          {children}
        </div>
        <div
          className="flex flex-wrap items-center justify-end gap-2 border-t px-5 py-3.5"
          hidden={!editable}
        >
          {errors.form ? (
            <div className="mr-auto">
              <FormError>{errors.form}</FormError>
            </div>
          ) : null}
          <Button
            type="button"
            variant="ghost"
            size="sm"
            disabled={!patch || busy}
            onClick={() => {
              setDraft(initial());
              setErrors({});
            }}
          >
            {t('settings.reset')}
          </Button>
          <Button type="submit" size="sm" disabled={!patch || busy}>
            {busy ? t('settings.saving') : t('settings.save')}
          </Button>
        </div>
      </form>
      <ConfirmDialog
        open={confirming !== null}
        onOpenChange={(o) => !o && setConfirming(null)}
        title={t('settings.confirmRestart.title')}
        description={t('settings.confirmRestart.body')}
        icon={TriangleAlert}
        tone="info"
        destructive={false}
        confirmLabel={t('settings.confirmRestart.confirm')}
        onConfirm={async () => {
          const p = confirming;
          setConfirming(null);
          // A refusal shows on its field, under the dialog that just closed.
          if (p) await commit(p).catch(() => {});
        }}
      />
    </Card>
  );
}

function FieldRow({
  id,
  spec,
  settingId,
  settings,
  value,
  onChange,
  error,
}: {
  id: string;
  spec: FieldSpec;
  settingId: SettingId;
  settings: AdminSettings;
  value: DraftValue;
  onChange: (v: DraftValue) => void;
  error?: string;
}) {
  const { t } = useTranslation();
  const label = t(`settings.${settingId}`);
  const body = t(`settings.${settingId}Body`, { defaultValue: '' });
  const locked = Boolean(lockOf(settings, settingId));
  const ariaDescribedBy = describedBy(id, Boolean(error), Boolean(body));
  const wide = spec.kind === 'list' || spec.kind === 'radio';

  let control: React.ReactNode;
  switch (spec.kind) {
    case 'switch':
      control = (
        <Switch
          id={id}
          checked={value as boolean}
          disabled={locked}
          onCheckedChange={(v) => onChange(v)}
          aria-describedby={ariaDescribedBy}
        />
      );
      break;
    case 'schedule':
      control = (
        <ScheduleInput
          id={id}
          value={value as string}
          onChange={onChange}
          disabled={locked}
          describedBy={ariaDescribedBy}
          invalid={Boolean(error)}
        />
      );
      break;
    case 'select':
      control = (
        <NativeSelect
          id={id}
          className="w-full sm:w-[260px]"
          value={value as string}
          disabled={locked}
          onChange={(e) => onChange(e.target.value)}
          aria-describedby={ariaDescribedBy}
          aria-invalid={error ? true : undefined}
        >
          {spec.options.map((o) => (
            <option key={o.value} value={o.value}>
              {o.label}
            </option>
          ))}
        </NativeSelect>
      );
      break;
    case 'radio':
      control = (
        <RadioCards
          label={label}
          value={value as string}
          onValueChange={(v) => onChange(v)}
          options={spec.options.map((o) => ({ ...o, disabled: locked || o.disabled }))}
          className="grid grid-cols-1 gap-2 md:grid-cols-3"
        />
      );
      break;
    case 'list':
      control = (
        <Textarea
          id={id}
          rows={3}
          className="w-full font-mono text-[12.5px]"
          value={value as string}
          placeholder={spec.placeholder}
          disabled={locked}
          spellCheck={false}
          onChange={(e) => onChange(e.target.value)}
          aria-describedby={ariaDescribedBy}
          aria-invalid={error ? true : undefined}
        />
      );
      break;
    default:
      control = (
        <Input
          id={id}
          type="text"
          inputMode={spec.kind === 'number' ? 'numeric' : undefined}
          className={cn('w-full sm:w-[300px]', spec.mono && 'font-mono text-[12.5px]')}
          value={value as string}
          placeholder={spec.placeholder}
          disabled={locked}
          spellCheck={false}
          onChange={(e) => onChange(e.target.value)}
          aria-describedby={ariaDescribedBy}
          aria-invalid={error ? true : undefined}
        />
      );
  }

  return (
    <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-2.5 py-4">
      <div className={cn('flex min-w-0 flex-1 basis-60 flex-col gap-0.5', wide && 'basis-full')}>
        <div className="flex flex-wrap items-center gap-2">
          {spec.kind === 'radio' ? (
            <b className="font-semibold">{label}</b>
          ) : (
            <label htmlFor={id} className="font-semibold">
              {label}
            </label>
          )}
          <SettingBadges settings={settings} id={settingId} />
        </div>
        {body ? (
          <span id={`${id}-desc`} className="text-[12.5px] text-muted-foreground">
            {body}
          </span>
        ) : null}
      </div>
      <div
        className={cn('flex min-w-0 flex-col gap-1.5', wide ? 'basis-full' : 'max-sm:basis-full')}
      >
        {control}
        {error ? (
          <p id={`${id}-error`} role="alert" className="text-[12.5px] font-medium text-destructive">
            {error}
          </p>
        ) : null}
      </div>
    </div>
  );
}

/** A switch that saves the moment it flips (no footer), restoring itself on a failure. */
export function InstantSwitch({
  id,
  checked,
  disabled,
  patch,
  onSaved,
  failedTitle,
  describedBy,
}: {
  id: string;
  checked: boolean;
  disabled?: boolean;
  patch: (on: boolean) => SettingsPatch;
  onSaved: (on: boolean) => void;
  failedTitle: string;
  describedBy?: string;
}) {
  const qc = useQueryClient();
  const save = useSaveSettings();
  const toggle = async (on: boolean) => {
    const before = qc.getQueryData<AdminSettings>(keys.settings);
    const p = patch(on);
    // Show the new position at once; put it back if the save fails.
    if (before) {
      const shown: Record<string, unknown> = { ...before };
      for (const [section, fields] of Object.entries(p)) {
        shown[section] = { ...before[section as SettingsSection], ...fields };
      }
      qc.setQueryData(keys.settings, shown as unknown as AdminSettings);
    }
    try {
      await save(p);
      onSaved(on);
    } catch (err) {
      qc.setQueryData(keys.settings, before);
      toastError(failedTitle, err);
    }
  };
  return (
    <Switch
      id={id}
      checked={checked}
      disabled={disabled}
      onCheckedChange={(v) => void toggle(v)}
      aria-describedby={describedBy}
    />
  );
}
