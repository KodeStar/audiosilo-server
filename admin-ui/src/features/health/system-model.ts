import type { SystemStatus } from '@/api/types';
import { certificateLook } from '@/features/settings/settings-model';

// Health > System: everything the server depends on as one list of rows, each
// with a status. Pure, so the rules (what counts as a problem) are tested
// without rendering; the page maps keys to words and icons.

export type RowStatus = 'ok' | 'warn' | 'bad' | 'off';

export interface SystemRow {
  /** Stable key, also the icon choice: tool, metadata, tls, database, library, player, update. */
  kind: 'ffmpeg' | 'ffprobe' | 'metadata' | 'tls' | 'database' | 'library' | 'player' | 'update';
  id: string;
  /** i18n key + values of the row's title (a library's name is passed through). */
  title: { key: string; values?: Record<string, string | number> };
  /** i18n key + values of the one-line detail (`free`/`total` are bytes, formatted by the page). */
  detail: { key: string; values?: Record<string, string | number> };
  /** A short mono value on the right (a version, a path), or "". */
  value: string;
  status: RowStatus;
  /** The status word's i18n key. */
  statusKey: string;
}

/** Free space under this share of a disk is worth a warning. */
export const LOW_DISK = 0.1;

export function systemRows(sys: SystemStatus, now: number = Date.now()): SystemRow[] {
  const rows: SystemRow[] = [];

  for (const tool of sys.tools) {
    rows.push({
      kind: tool.name,
      id: tool.name,
      title: { key: `system.row.${tool.name}` },
      detail: tool.path
        ? {
            key:
              tool.source === 'downloaded'
                ? `system.detail.${tool.name}Downloaded`
                : `system.detail.${tool.name}`,
          }
        : { key: `system.detail.${tool.name}Missing` },
      value: tool.path ? tool.version : '',
      status: tool.path ? 'ok' : 'warn',
      statusKey: tool.path ? 'system.status.ok' : 'system.status.missing',
    });
  }

  const m = sys.metadata;
  const metaRow: SystemRow = {
    kind: 'metadata',
    id: 'metadata',
    title: { key: 'system.row.metadata' },
    detail: { key: 'system.detail.metadataOff' },
    value: hostOf(m.base_url),
    status: 'off',
    statusKey: 'system.status.off',
  };
  if (!m.available) {
    metaRow.detail = { key: 'system.detail.metadataUnavailable' };
  } else if (m.enabled && m.health) {
    metaRow.status = m.health.reachable ? 'ok' : 'bad';
    metaRow.statusKey = m.health.reachable ? 'system.status.ok' : 'system.status.attention';
    metaRow.detail = m.health.reachable
      ? { key: 'system.detail.metadataOk', values: { ms: m.health.latency_ms } }
      : { key: 'system.detail.metadataDown' };
  }
  rows.push(metaRow);

  const look = certificateLook(sys.tls, now);
  const first = sys.tls.certificates.find((c) => c.issued);
  rows.push({
    kind: 'tls',
    id: 'tls',
    title: { key: 'system.row.tls' },
    detail: look
      ? { key: look.key, values: { days: look.days ?? 0, count: look.days ?? 0 } }
      : { key: 'system.detail.tlsOff' },
    value: first ? first.issuer : '',
    status: !look
      ? 'off'
      : look.tone === 'success'
        ? 'ok'
        : look.tone === 'destructive'
          ? 'bad'
          : look.tone === 'warning'
            ? 'warn'
            : 'off',
    statusKey: !look
      ? 'system.status.off'
      : look.tone === 'success'
        ? 'system.status.ok'
        : look.tone === 'secondary'
          ? 'system.status.waiting'
          : 'system.status.attention',
  });

  rows.push({
    kind: 'database',
    id: 'database',
    title: { key: 'system.row.database' },
    detail: {
      key: 'system.detail.database',
      values: { schema: schemaNumber(sys.database.schema) },
    },
    value: '',
    status: 'ok',
    statusKey: 'system.status.ok',
  });

  for (const lib of sys.libraries) {
    const low = lib.disk && lib.disk.total > 0 && lib.disk.free / lib.disk.total < LOW_DISK;
    rows.push({
      kind: 'library',
      id: `library-${lib.id}`,
      title: { key: 'system.row.library', values: { name: lib.name } },
      detail: !lib.available
        ? { key: 'system.detail.libraryOffline' }
        : lib.disk
          ? {
              key: low ? 'system.detail.diskLow' : 'system.detail.disk',
              values: { free: lib.disk.free, total: lib.disk.total },
            }
          : { key: 'system.detail.libraryOk' },
      value: lib.root,
      status: !lib.available ? 'bad' : low ? 'warn' : 'ok',
      statusKey: !lib.available || low ? 'system.status.attention' : 'system.status.ok',
    });
  }

  rows.push({
    kind: 'player',
    id: 'player',
    title: { key: 'system.row.player' },
    detail: { key: `settings.players.source.${sys.web_player || 'none'}Body` },
    value: '',
    status: sys.web_player ? 'ok' : 'off',
    statusKey: sys.web_player ? 'system.status.ok' : 'system.status.off',
  });

  const u = sys.update;
  rows.push({
    kind: 'update',
    id: 'update',
    title: { key: 'system.row.update' },
    detail: !u.enabled
      ? { key: 'system.detail.updateOff' }
      : u.error
        ? { key: `about.error.${u.error}` }
        : u.update_available && u.latest
          ? { key: 'system.detail.updateAvailable', values: { version: u.latest.version } }
          : u.checked_at
            ? { key: 'system.detail.upToDate' }
            : { key: 'system.detail.notChecked' },
    value: u.current,
    status: !u.enabled ? 'off' : u.error || u.update_available ? 'warn' : 'ok',
    statusKey: !u.enabled
      ? 'system.status.off'
      : u.update_available
        ? 'system.status.update'
        : u.error
          ? 'system.status.attention'
          : 'system.status.ok',
  });

  return rows;
}

/** The worst status of a set of rows (the page's headline). */
export function worstStatus(rows: SystemRow[]): RowStatus {
  if (rows.some((r) => r.status === 'bad')) return 'bad';
  if (rows.some((r) => r.status === 'warn')) return 'warn';
  return 'ok';
}

/** "0018_sessions.sql" -> 18. */
export function schemaNumber(name: string): number {
  return Number.parseInt(name, 10) || 0;
}

function hostOf(url: string): string {
  try {
    return new URL(url).host;
  } catch {
    return url;
  }
}

/** How long the server has been up, in whole units: [n, unit]. */
export function uptime(
  startedAt: string,
  now: number = Date.now(),
): [number, 'minute' | 'hour' | 'day'] {
  const minutes = Math.max(0, Math.floor((now - Date.parse(startedAt)) / 60_000));
  if (minutes < 60) return [minutes, 'minute'];
  const hours = Math.floor(minutes / 60);
  if (hours < 48) return [hours, 'hour'];
  return [Math.floor(hours / 24), 'day'];
}
