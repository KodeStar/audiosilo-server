import type { NotifyTarget, NotifyTargetKind, ServerEventKind } from '@/api/types';

// Settings > Notifications: what each kind of destination asks for, and how a
// destination's last delivery reads. Pure, so the rules are tested on their own.

export const TARGET_KINDS: readonly NotifyTargetKind[] = ['webhook', 'ntfy', 'discord'];

/** What a destination of each kind asks for: an example address, and its secret if it takes one. */
export const TARGET_FORM: Record<
  NotifyTargetKind,
  { placeholder: string; secret: 'signing' | 'token' | null }
> = {
  webhook: { placeholder: 'https://hooks.example.com/audiosilo', secret: 'signing' },
  ntfy: { placeholder: 'https://ntfy.sh/your-topic', secret: 'token' },
  discord: { placeholder: 'https://discord.com/api/webhooks/…', secret: null },
};

/** What a new destination is sent until the admin chooses: the problems, not the chatter. */
export const DEFAULT_EVENTS: readonly ServerEventKind[] = [
  'scan_failed',
  'library_unavailable',
  'update_available',
  'backup_failed',
];

/** A failure reason's i18n key and values ("timeout", "unreachable", "http_404", "failed"). */
export function reasonText(reason: string): { key: string; values?: Record<string, string> } {
  const http = /^http_(\d{3})$/.exec(reason);
  if (http) return { key: 'notify.reason.http', values: { status: http[1] } };
  if (reason === 'timeout' || reason === 'unreachable') return { key: `notify.reason.${reason}` };
  return { key: 'notify.reason.failed' };
}

/** How a destination's last delivery reads: never tried, arrived, or failed (and why). */
export function deliveryLook(t: NotifyTarget): {
  tone: 'ok' | 'bad' | 'off';
  key: string;
  reason?: { key: string; values?: Record<string, string> };
} {
  if (!t.last_at) return { tone: 'off', key: 'notify.delivery.never' };
  if (t.last_ok) return { tone: 'ok', key: 'notify.delivery.ok' };
  return { tone: 'bad', key: 'notify.delivery.failed', reason: reasonText(t.last_error) };
}

/** The events with one kind switched on or off, in the server's order. */
export function toggleEvent(
  events: readonly ServerEventKind[],
  kind: ServerEventKind,
  on: boolean,
  order: readonly ServerEventKind[],
): ServerEventKind[] {
  const set = new Set(events);
  if (on) set.add(kind);
  else set.delete(kind);
  return order.filter((k) => set.has(k));
}
