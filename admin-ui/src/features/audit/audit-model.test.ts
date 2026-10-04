import { describe, expect, it } from 'vitest';
import type { AuditEvent } from '@/api/types';
import { actionText, detailLines, valueText } from './audit-model';

// A translator that shows which key was asked for (and the default when given).
const known = new Set(['audit.enum.password.set', 'audit.enum.role.admin']);
const t = (key: string, opts?: Record<string, unknown>) => {
  if (known.has(key)) return key;
  if (opts && 'defaultValue' in opts) {
    return key === 'audit.action.user.update' ? 'Changed an account' : String(opts.defaultValue);
  }
  if (key === 'audit.value.change') return `${String(opts?.from)} → ${String(opts?.to)}`;
  if (key === 'audit.value.more') return `and ${String(opts?.n)} more`;
  if (key === 'audit.action.other') return `Other: ${String(opts?.action)}`;
  return key.split('.').pop() ?? key;
};
const fmt = {
  number: (n: number) => n.toLocaleString('en'),
  date: (iso: string) => `date(${iso})`,
};

const ev = (action: string, details: Record<string, unknown>): AuditEvent => ({
  id: 1,
  at: '2026-10-04T15:01:00.000Z',
  actor_id: 1,
  actor_name: 'chris',
  via: 'session',
  action,
  target: 'x',
  details,
});

describe('audit model', () => {
  it('words known actions and names unknown ones', () => {
    expect(actionText(ev('user.update', {}), t)).toBe('Changed an account');
    expect(actionText(ev('future.thing', {}), t)).toBe('Other: future.thing');
  });

  it('formats values', () => {
    expect(valueText(true, t, fmt)).toBe('yes');
    expect(valueText('', t, fmt)).toBe('none');
    expect(valueText([], t, fmt)).toBe('none');
    expect(valueText(['a', 'b'], t, fmt)).toBe('a, b');
    expect(valueText(1200, t, fmt)).toBe('1,200');
  });

  it('lists a settings save as each setting from and to', () => {
    const lines = detailLines(
      ev('settings.update', {
        changes: [
          { setting: 'backups.keep', from: 7, to: 14 },
          { setting: 'network.cors_origins', from: [], to: ['http://localhost:8081'] },
        ],
      }),
      t,
      fmt,
    );
    expect(lines).toEqual([
      { label: 'backups.keep', value: '7 → 14' },
      { label: 'network.cors_origins', value: 'none → http://localhost:8081' },
    ]);
  });

  it('words coded values, keeping an unknown code as is', () => {
    expect(detailLines(ev('user.update', { password: 'set', role: 'admin' }), t, fmt)).toEqual([
      { label: 'password', value: 'audit.enum.password.set' },
      { label: 'role', value: 'audit.enum.role.admin' },
    ]);
    expect(detailLines(ev('library.folder_override', { mode: 'sideways' }), t, fmt)).toEqual([
      { label: 'mode', value: 'sideways' },
    ]);
  });

  it('names events and writes times', () => {
    expect(
      detailLines(
        ev('notify.create', { events: ['scan_failed'], expires_at: '2026-10-11T17:42:05Z' }),
        t,
        fmt,
      ),
    ).toEqual([
      { label: 'events', value: 'scan_failed' },
      { label: 'expires_at', value: 'date(2026-10-11T17:42:05Z)' },
    ]);
  });

  it('lists a book edit field by field, and paths as the first few and a count', () => {
    expect(
      detailLines(ev('book.edit', { set: { title: 'Dune' }, source: 'community' }), t, fmt),
    ).toEqual([
      { label: 'title', value: 'Dune' },
      { label: 'source', value: 'community' },
    ]);
    expect(
      detailLines(
        ev('share.add_paths', { paths: { count: 12, first: ['Fiction: Dune', 'Kids'] } }),
        t,
        fmt,
      ),
    ).toEqual([{ label: 'paths', value: 'Fiction: Dune, Kids and 10 more' }]);
  });
});
