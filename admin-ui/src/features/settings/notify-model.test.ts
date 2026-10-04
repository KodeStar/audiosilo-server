import { describe, expect, it } from 'vitest';
import type { NotifyTarget } from '@/api/types';
import { EVENT_KINDS } from '@/lib/server-events';
import { deliveryLook, reasonText, toggleEvent } from './notify-model';

const target = (over: Partial<NotifyTarget>): NotifyTarget => ({
  id: 1,
  kind: 'webhook',
  name: 'Home',
  address: 'https://hooks.example.com/aud…',
  has_secret: false,
  enabled: true,
  events: [],
  created_at: '',
  updated_at: '',
  last_at: null,
  last_ok: null,
  last_error: '',
  ...over,
});

describe('notify model', () => {
  it('words failure reasons, unknown ones generically', () => {
    expect(reasonText('http_404')).toEqual({
      key: 'notify.reason.http',
      values: { status: '404' },
    });
    expect(reasonText('timeout')).toEqual({ key: 'notify.reason.timeout' });
    expect(reasonText('unreachable')).toEqual({ key: 'notify.reason.unreachable' });
    expect(reasonText('http_99999')).toEqual({ key: 'notify.reason.failed' });
    expect(reasonText('')).toEqual({ key: 'notify.reason.failed' });
  });

  it('reads the last delivery', () => {
    expect(deliveryLook(target({}))).toEqual({ tone: 'off', key: 'notify.delivery.never' });
    expect(deliveryLook(target({ last_at: 'x', last_ok: true }))).toEqual({
      tone: 'ok',
      key: 'notify.delivery.ok',
    });
    expect(deliveryLook(target({ last_at: 'x', last_ok: false, last_error: 'http_500' }))).toEqual({
      tone: 'bad',
      key: 'notify.delivery.failed',
      reason: { key: 'notify.reason.http', values: { status: '500' } },
    });
  });

  it('switches an event on or off in the server order', () => {
    expect(toggleEvent(['backup_failed'], 'book_added', true, EVENT_KINDS)).toEqual([
      'book_added',
      'backup_failed',
    ]);
    expect(toggleEvent(['book_added', 'backup_failed'], 'book_added', false, EVENT_KINDS)).toEqual([
      'backup_failed',
    ]);
    expect(toggleEvent(['book_added'], 'book_added', true, EVENT_KINDS)).toEqual(['book_added']);
  });
});
