import { useEffect, useLayoutEffect, useRef, useState } from 'react';
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
import { ProvenanceMarker, UnsavedMarker } from '@/components/provenance';
import { Card, CardHeader } from '@/components/ui/card';
import { toastError } from '@/lib/errors';
import { toast } from '@/lib/toast';
import { cn } from '@/lib/utils';
import { undoRevertRequest, type Drafts } from './book-model';

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

  const undo = async (field: OverrideField, before: FieldValue) => {
    const label = t(`book.field.${field}`);
    try {
      settleBookEdit(
        qc,
        await api.editBook(b.library_id, b.path, undoRevertRequest(field, before)),
      );
      toast.add({ title: t('book.revert.undone', { field: label }), type: 'success' });
    } catch (err) {
      toastError(t('book.revert.undoFailed', { field: label }), err);
    }
  };

  const revert = async (field: OverrideField) => {
    const before = detail.fields[field];
    const label = t(`book.field.${field}`);
    try {
      const next = await api.editBook(b.library_id, b.path, { revert: [field] });
      settleBookEdit(qc, next);
      const now = next.fields[field]?.value ?? '';
      toast.add({
        title: t('book.revert.done', { field: label }),
        description: now
          ? t('book.revert.doneBody', { value: quote(now) })
          : t('book.revert.doneEmpty'),
        type: 'success',
        actionProps: { children: t('book.revert.undo'), onClick: () => void undo(field, before) },
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

/** One field: label, the click-to-edit value, and where the value came from. */
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
  const shown = draft ?? value.value;
  const empty = !shown.trim();
  const label = t(`book.field.${field}`);
  const id = `field-${field}`;
  const long = field === 'description';
  const mono = field === 'asin' || field === 'isbn';

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
      {editing ? (
        <FieldEditor
          id={id}
          label={label}
          initial={shown}
          long={long}
          mono={mono}
          invalid={!!error}
          onDone={finish}
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
          <span id={`${id}-value`} className={cn(long && 'line-clamp-4 whitespace-pre-line')}>
            {empty ? t('book.field.empty') : shown}
          </span>
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
                ? t('book.revert.tagHint', { value: quote(value.scanned) })
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

const editorClass =
  'w-full min-w-0 rounded-md border border-input bg-card px-3 text-sm outline-none focus-visible:border-ring focus-visible:shadow-[0_0_0_3px_color-mix(in_oklab,var(--ring)_20%,transparent)] aria-invalid:border-destructive';

/**
 * The value being edited: Enter commits (Ctrl/Cmd+Enter in the description,
 * where Enter is a new line), Escape cancels, leaving the field commits.
 */
function FieldEditor({
  id,
  label,
  initial,
  long,
  mono,
  invalid,
  onDone,
}: {
  id: string;
  label: string;
  initial: string;
  long: boolean;
  mono: boolean;
  invalid: boolean;
  /** raw undefined = cancelled; keyboard = Enter/Escape (focus returns to the value). */
  onDone: (raw: string | undefined, keyboard: boolean) => void;
}) {
  const ref = useRef<HTMLInputElement & HTMLTextAreaElement>(null);
  // Enter or Escape ends the edit before the blur that unmounting can fire.
  const ended = useRef(false);
  // Before paint, so a key pressed in the first frame isn't overwritten by select().
  useLayoutEffect(() => {
    ref.current?.focus();
    ref.current?.select();
  }, []);
  const end = (raw: string | undefined, keyboard: boolean) => {
    if (ended.current) return;
    ended.current = true;
    onDone(raw, keyboard);
  };
  const props = {
    ref,
    id,
    defaultValue: initial,
    'aria-label': label,
    'aria-invalid': invalid || undefined,
    'aria-describedby': invalid ? `${id}-error` : undefined,
    onBlur: (e: React.FocusEvent<HTMLInputElement | HTMLTextAreaElement>) =>
      end(e.currentTarget.value, false),
    onKeyDown: (e: React.KeyboardEvent<HTMLInputElement | HTMLTextAreaElement>) => {
      if (e.key === 'Escape') {
        e.preventDefault();
        e.stopPropagation();
        end(undefined, true);
      } else if (e.key === 'Enter' && (!long || e.metaKey || e.ctrlKey)) {
        e.preventDefault();
        end(e.currentTarget.value, true);
      }
    },
  };
  return long ? (
    <textarea {...props} rows={5} className={cn(editorClass, 'py-2 leading-[1.55]')} />
  ) : (
    <input {...props} className={cn(editorClass, 'h-[38px]', mono && 'font-mono')} />
  );
}
