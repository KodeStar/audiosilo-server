import type { AuditEvent } from '@/api/types';
import { describeSchedule } from '@/features/settings/backups-model';

// Server > Audit log: an admin action's code and facts (catalog.AuditEvent) as
// the page words them. Pure: the page passes the translator in.

/** The action areas the log can be narrowed to (an action's first part), in menu order. */
export const AUDIT_AREAS = [
  'user',
  'invite',
  'device',
  'share',
  'progress',
  'library',
  'book',
  'issue',
  'settings',
  'backup',
  'notify',
] as const;

/** i18next's t as far as these rules use it (options are optional, unlike lib/format's). */
type Translate = (key: string, opts?: Record<string, unknown>) => string;

/** Details whose values are codes with words of their own (`audit.enum.<key>.<value>`). */
const ENUMS = new Set(['password', 'mode', 'kind', 'source', 'role', 'error']);

/** How numbers and times are written in the console's language. */
export interface Formatters {
  number: (n: number) => string;
  /** An ISO time as a date and time. */
  date: (iso: string) => string;
}

/** One fact of an event as a label and a value, both already in words. */
export interface DetailLine {
  label: string;
  value: string;
}

/** The action in words: its own key, or a generic one naming the code (a newer server's action). */
export function actionText(e: AuditEvent, t: Translate): string {
  return (
    t(`audit.action.${e.action}`, { defaultValue: '' }) ||
    t('audit.action.other', { action: e.action })
  );
}

/** A value as one short string: lists joined, booleans as yes/no, empty as "none". */
export function valueText(v: unknown, t: Translate, fmt: Formatters): string {
  if (v === null || v === undefined || v === '') return t('audit.value.none');
  if (typeof v === 'boolean') return v ? t('audit.value.yes') : t('audit.value.no');
  if (typeof v === 'number') return fmt.number(v);
  if (Array.isArray(v))
    return v.length ? v.map((x) => valueText(x, t, fmt)).join(', ') : t('audit.value.none');
  if (typeof v === 'object') {
    return Object.entries(v as Record<string, unknown>)
      .map(([k, x]) => `${k}: ${valueText(x, t, fmt)}`)
      .join(' · ');
  }
  return String(v);
}

/**
 * An event's details as labelled lines. A settings save lists each setting
 * "from → to" under its own name; a book edit lists each field it set; a share's
 * paths read as their count and the first few; anything else is key: value, with
 * labels from `audit.detail.<key>` (the key itself when the console doesn't know it).
 */
export function detailLines(e: AuditEvent, t: Translate, fmt: Formatters): DetailLine[] {
  const out: DetailLine[] = [];
  const label = (key: string) => t(`audit.detail.${key}`, { defaultValue: key });
  for (const [key, v] of Object.entries(e.details)) {
    if (key === 'changes' && Array.isArray(v)) {
      for (const c of v as { setting?: unknown; from?: unknown; to?: unknown }[]) {
        const id = String(c.setting ?? '');
        // A backup schedule is stored as code ("daily:03:00"): word it.
        const word = (x: unknown) =>
          id === 'backups.schedule' && typeof x === 'string'
            ? scheduleText(x, t)
            : valueText(x, t, fmt);
        out.push({
          label: t(`settings.${id}`, { defaultValue: id }),
          value: t('audit.value.change', { from: word(c.from), to: word(c.to) }),
        });
      }
      continue;
    }
    if (key === 'set' && v && typeof v === 'object' && !Array.isArray(v)) {
      for (const [field, value] of Object.entries(v as Record<string, unknown>)) {
        out.push({
          label: t(`book.field.${field}`, { defaultValue: field }),
          value: valueText(value, t, fmt),
        });
      }
      continue;
    }
    if (key === 'paths' && v && typeof v === 'object' && !Array.isArray(v)) {
      const p = v as { count?: unknown; first?: unknown };
      const first = Array.isArray(p.first) ? p.first.map(String) : [];
      const count = typeof p.count === 'number' ? p.count : first.length;
      const more = count - first.length;
      out.push({
        label: label('paths'),
        value:
          first.join(', ') +
          (more > 0 ? ` ${t('audit.value.more', { count: more, n: fmt.number(more) })}` : ''),
      });
      continue;
    }
    let value: string;
    if (key === 'kind' && e.action.startsWith('issue.') && typeof v === 'string')
      value = t(`health.kind.${v}`, { defaultValue: v });
    else if (ENUMS.has(key) && typeof v === 'string')
      value = t(`audit.enum.${key}.${v}`, { defaultValue: v });
    else if (key === 'events' && Array.isArray(v))
      value = v.length
        ? v.map((k) => t(`notify.event.${String(k)}`, { defaultValue: String(k) })).join(', ')
        : t('audit.value.none');
    else if (key.endsWith('_at') && typeof v === 'string') value = fmt.date(v) || v;
    else value = valueText(v, t, fmt);
    out.push({ label: label(key), value });
  }
  return out;
}

function scheduleText(s: string, t: Translate): string {
  const d = describeSchedule(s);
  return t(d.key, d.values);
}
