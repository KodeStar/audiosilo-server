import { useEffect, useRef, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { Undo2 } from 'lucide-react';
import { api } from '@/api/client';
import { settleBookEdit } from '@/api/hooks';
import {
  FIELD_SOURCES,
  OVERRIDE_FIELDS,
  type AdminBookDetail,
  type FieldValue,
  type OverrideField,
} from '@/api/types';
import { InlineEdit } from '@/components/inline-edit';
import { LabelInput } from '@/components/label-input';
import { ProvenanceMarker, UnsavedMarker } from '@/components/provenance';
import { Card, CardHeader } from '@/components/ui/card';
import { NativeSelect } from '@/components/ui/native-select';
import { toastError } from '@/lib/errors';
import { encodeLabels, parseLabels } from '@/lib/labels';
import { toast } from '@/lib/toast';
import { cn } from '@/lib/utils';
import { displayValue, shownValue, undoRevertRequest, type Drafts } from './book-model';

/** How much of a value a toast quotes. */
const QUOTE = 60;
const quote = (s: string) => (s.length > QUOTE ? `${s.slice(0, QUOTE - 3)}...` : s);

/**
 * The Details card (STYLEGUIDE.md "Provenance marker + Field row"): one row per
 * overridable field, edited in place. A commit only drafts the change (the
 * save bar saves them together); a locked field's Revert saves at once, with
 * an Undo.
 */
export function DetailsCard({
  detail,
  drafts,
  errors,
  onCommit,
}: {
  detail: AdminBookDetail;
  drafts: Drafts;
  /** Messages under fields: what's wrong with a draft, or why the server refused it. */
  errors: Partial<Record<OverrideField, string>>;
  onCommit: (field: OverrideField, raw: string) => void;
}) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const b = detail.book;

  const undo = async (
    field: OverrideField,
    before: Record<OverrideField, FieldValue>,
    after: Record<OverrideField, FieldValue>,
  ) => {
    const label = t(`book.field.${field}`);
    try {
      settleBookEdit(
        qc,
        await api.editBook(b.library_id, b.path, undoRevertRequest(field, before, after)),
      );
      toast.add({ title: t('book.revert.undone', { field: label }), type: 'success' });
    } catch (err) {
      toastError(t('book.revert.undoFailed', { field: label }), err);
    }
  };

  const revert = async (field: OverrideField) => {
    const before = detail.fields;
    const label = t(`book.field.${field}`);
    try {
      const next = await api.editBook(b.library_id, b.path, { revert: [field] });
      settleBookEdit(qc, next);
      const now = shownValue(t, field, next.fields[field]?.value ?? '');
      toast.add({
        title: t('book.revert.done', { field: label }),
        description: now
          ? t('book.revert.doneBody', { value: quote(now) })
          : t('book.revert.doneEmpty'),
        type: 'success',
        actionProps: {
          children: t('book.revert.undo'),
          onClick: () => void undo(field, before, next.fields),
        },
      });
    } catch (err) {
      toastError(t('book.revert.failed', { field: label }), err);
    }
  };

  return (
    <Card aria-labelledby="details-title">
      <CardHeader
        titleId="details-title"
        title={t('book.details.title')}
        description={t('book.details.description')}
      />
      <div className="px-5 py-2">
        {OVERRIDE_FIELDS.map((f) => (
          <FieldRow
            key={f}
            field={f}
            value={detail.fields[f]}
            draft={drafts[f]}
            error={errors[f]}
            onCommit={(raw) => onCommit(f, raw)}
            onRevert={() => void revert(f)}
          />
        ))}
      </div>
      <div
        className="flex flex-wrap items-center gap-x-3.5 gap-y-2 border-t px-5 py-3 text-[12px]"
        role="note"
        aria-label={t('book.legend.title')}
      >
        <span className="text-muted-foreground">{t('book.legend.title')}</span>
        {FIELD_SOURCES.map((s) => (
          <span key={s} className="inline-flex items-center gap-1.5">
            <ProvenanceMarker source={s} />
            <span className="text-subtle-foreground">{t(`book.legend.${s}`)}</span>
          </span>
        ))}
      </div>
    </Card>
  );
}

/** The values the abridged field takes, unknown first. */
const ABRIDGED = ['', 'true', 'false'] as const;

/**
 * One field: label, the value (click to edit; a label list as chips, edited as
 * chips; abridged a choice of three), and where the value came from.
 */
function FieldRow({
  field,
  value,
  draft,
  error,
  onCommit,
  onRevert,
}: {
  field: OverrideField;
  value: FieldValue;
  draft: string | undefined;
  error: string | undefined;
  onCommit: (raw: string) => void;
  onRevert: () => void;
}) {
  const { t } = useTranslation();
  const [editing, setEditing] = useState(false);
  const button = useRef<HTMLButtonElement>(null);
  const refocus = useRef(false);
  const dirty = draft !== undefined;
  // A draft is what was typed; a saved value is shown as it is edited.
  const shown = draft ?? displayValue(field, value.value);
  const empty = !shown.trim();
  const label = t(`book.field.${field}`);
  const id = `field-${field}`;
  const long = field === 'description';
  const mono = field === 'asin' || field === 'isbn';
  const labels = field === 'genres' || field === 'tags';
  // A label list's draft is its JSON (LabelInput's list), shown as chips.
  const chips = labels ? parseLabels(draft ?? value.value) : [];

  // After Enter or Escape, keyboard focus goes back to the value.
  useEffect(() => {
    if (!editing && refocus.current) {
      refocus.current = false;
      button.current?.focus();
    }
  }, [editing]);

  const finish = (raw: string | undefined, keyboard: boolean) => {
    refocus.current = keyboard;
    setEditing(false);
    if (raw !== undefined) onCommit(raw);
  };

  return (
    <div
      role="group"
      aria-labelledby={`${id}-label`}
      className={cn(
        '-mx-3 grid min-h-12 grid-cols-[minmax(0,1fr)_auto] items-center gap-x-2.5 gap-y-1 rounded-[12px] px-3 py-[9px] transition-colors duration-(--dur-1) md:grid-cols-[130px_minmax(0,1fr)_auto] md:gap-x-4',
        dirty ? 'bg-[color-mix(in_oklab,var(--brand)_6%,transparent)]' : 'hover:bg-muted',
      )}
    >
      <span
        id={`${id}-label`}
        className="col-span-full text-[12.5px] font-[550] text-muted-foreground md:col-span-1"
      >
        {label}
      </span>
      {field === 'abridged' ? (
        <NativeSelect
          id={id}
          value={draft ?? value.value}
          onChange={(e) => onCommit(e.target.value)}
          aria-labelledby={`${id}-label`}
          aria-invalid={!!error || undefined}
          aria-describedby={error ? `${id}-error` : undefined}
          className="max-w-[240px]"
        >
          {ABRIDGED.map((v) => (
            <option key={v} value={v}>
              {t(`book.abridged.${v || 'unknown'}`)}
            </option>
          ))}
        </NativeSelect>
      ) : editing && labels ? (
        <LabelsEdit
          id={id}
          initial={chips}
          label={label}
          error={error}
          onDone={(next, keyboard) =>
            finish(next === undefined ? undefined : encodeLabels(next), keyboard)
          }
        />
      ) : editing ? (
        <InlineEdit
          id={id}
          initial={shown}
          multiline={long}
          onDone={finish}
          aria-label={label}
          aria-invalid={!!error || undefined}
          aria-describedby={error ? `${id}-error` : undefined}
          className={cn(
            'px-3 text-sm',
            long ? 'py-2 leading-[1.55]' : 'h-[38px]',
            mono && 'font-mono',
          )}
        />
      ) : (
        <button
          ref={button}
          type="button"
          onClick={() => setEditing(true)}
          // Named "<field> <value>": a list of bare values says nothing about
          // which field each button edits.
          aria-labelledby={`${id}-label ${id}-value`}
          aria-describedby={error ? `${id}-error` : undefined}
          className={cn(
            '-mx-1.5 -my-1 min-w-0 cursor-text rounded-[8px] px-1.5 py-1 text-left text-[14.5px] font-[550] [overflow-wrap:anywhere] hover:bg-card hover:shadow-[inset_0_0_0_1px_var(--border-strong)]',
            empty && 'font-[450] text-subtle-foreground italic',
            !empty && long && 'text-[13.5px] leading-[1.55] font-[450] text-muted-foreground',
            !empty && mono && 'font-mono text-[13px]',
          )}
        >
          {labels && chips.length ? (
            <span id={`${id}-value`} className="flex flex-wrap gap-1.5">
              {chips.map((c) => (
                <span
                  key={c}
                  className="inline-flex h-[24px] items-center rounded-full bg-secondary px-2.5 text-[12.5px] font-[550] text-secondary-foreground"
                >
                  {c}
                </span>
              ))}
            </span>
          ) : (
            <span id={`${id}-value`} className={cn(long && 'line-clamp-4 whitespace-pre-line')}>
              {empty ? t('book.field.empty') : shown}
            </span>
          )}
        </button>
      )}
      <span className="flex items-center gap-1">
        {dirty ? <UnsavedMarker /> : <ProvenanceMarker source={value.source} />}
        {value.locked && !dirty ? (
          <button
            type="button"
            onClick={onRevert}
            title={
              value.scanned
                ? t('book.revert.tagHint', { value: quote(shownValue(t, field, value.scanned)) })
                : t('book.revert.noTag')
            }
            aria-label={t('book.revert.aria', { field: label })}
            className="inline-flex items-center gap-1 rounded-[6px] px-1.5 py-0.5 text-[11.5px] font-semibold text-muted-foreground hover:bg-accent hover:text-foreground"
          >
            <Undo2 className="size-3" aria-hidden="true" />
            {t('book.revert.label')}
          </button>
        ) : null}
      </span>
      {error ? (
        <p
          id={`${id}-error`}
          role="alert"
          className="col-span-full text-[12.5px] font-medium text-destructive md:col-start-2"
        >
          {error}
        </p>
      ) : null}
    </div>
  );
}

/**
 * A label list being edited in place: the chips and the box to add to them,
 * committed as a whole when the editor ends (LabelInput's onDone).
 */
function LabelsEdit({
  id,
  initial,
  label,
  error,
  onDone,
}: {
  id: string;
  initial: string[];
  label: string;
  error: string | undefined;
  onDone: (value: string[] | undefined, keyboard: boolean) => void;
}) {
  const { t } = useTranslation();
  const [list, setList] = useState(initial);
  return (
    <LabelInput
      id={id}
      value={list}
      onChange={setList}
      onDone={onDone}
      autoFocus
      placeholder={t('labels.placeholder')}
      aria-label={label}
      aria-invalid={!!error || undefined}
      aria-describedby={error ? `${id}-error` : undefined}
    />
  );
}
