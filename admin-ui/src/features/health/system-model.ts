import type { MetaMirrorStatus, SystemStatus } from '@/api/types';
import { certificateLook } from '@/features/settings/settings-model';
import { backupHealth } from '@/features/settings/backups-model';
import { hostOf } from '@/lib/format';
import { backupFailureKey } from '@/lib/server-events';

// Health > System: everything the server depends on as one list of rows, each
// with a status. Pure, so the rules (what counts as a problem) are tested
// without rendering; the page maps keys to words and icons.

export type RowStatus = 'ok' | 'warn' | 'bad' | 'off';

type Text = { key: string; values?: Record<string, string | number> };

export interface SystemRow {
  /** Stable key, also the icon choice. */
  kind:
    | 'ffmpeg'
    | 'ffprobe'
    | 'metadata'
    | 'tls'
    | 'database'
    | 'backups'
    | 'library'
    | 'player'
    | 'update';
  id: string;
  /** i18n key + values of the row's title (a library's name is passed through). */
  title: Text;
  /**
   * i18n key + values of the one-line detail (`free`/`total` are bytes and `at`/`next`
   * ISO times, formatted by the page).
   */
  detail: Text;
  /** A short mono value on the right (a version, a path), or "". */
  value: string;
  status: RowStatus;
  /** The status word's i18n key: the status's own word unless the row says better. */
  statusKey: string;
  /** The metadata row in mirror mode: the local copy's status, shown under the row. */
  mirror?: MetaMirrorStatus;
}

/** The word for each status, unless a row has a more specific one. */
const STATUS_WORD: Record<RowStatus, string> = {
  ok: 'system.status.ok',
  warn: 'system.status.attention',
  bad: 'system.status.attention',
  off: 'system.status.off',
};

/** Free space under this share of a disk is worth a warning. */
export const LOW_DISK = 0.1;

type RowInput = Omit<SystemRow, 'statusKey' | 'value'> & { value?: string; statusKey?: string };

const row = (r: RowInput): SystemRow => ({
  ...r,
  value: r.value ?? '',
  statusKey: r.statusKey ?? STATUS_WORD[r.status],
});

export function systemRows(sys: SystemStatus, now: number = Date.now()): SystemRow[] {
  const rows: SystemRow[] = sys.tools.map((tool) =>
    row({
      kind: tool.name,
      id: tool.name,
      title: { key: `system.row.${tool.name}` },
      detail: {
        key: !tool.path
          ? `system.detail.${tool.name}Missing`
          : tool.source === 'downloaded'
            ? `system.detail.${tool.name}Downloaded`
            : `system.detail.${tool.name}`,
      },
      value: tool.path ? tool.version : '',
      ...(tool.path ? { status: 'ok' } : { status: 'warn', statusKey: 'system.status.missing' }),
    }),
  );

  rows.push(metadataRow(sys));

  const look = certificateLook(sys.tls, now);
  rows.push(
    row({
      kind: 'tls',
      id: 'tls',
      title: { key: 'system.row.tls' },
      detail: look
        ? { key: look.key, values: { days: look.days ?? 0, count: look.days ?? 0 } }
        : { key: 'system.detail.tlsOff' },
      value: sys.tls.certificates.find((c) => c.issued)?.issuer,
      ...(!look
        ? { status: 'off' }
        : look.status === 'waiting'
          ? { status: 'off', statusKey: 'system.status.waiting' }
          : { status: look.status }),
    }),
  );

  rows.push(
    row({
      kind: 'database',
      id: 'database',
      title: { key: 'system.row.database' },
      detail: {
        key: 'system.detail.database',
        values: { schema: schemaNumber(sys.database.schema) },
      },
      status: 'ok',
    }),
  );

  const b = sys.backups;
  if (b) {
    const base = {
      kind: 'backups',
      id: 'backups',
      title: { key: 'system.row.backups' },
      value: b.dir,
    } as const;
    const at = b.latest?.created_at ?? '';
    switch (backupHealth(b)) {
      case 'failed':
        rows.push(
          row({
            ...base,
            detail: { key: backupFailureKey(b.last?.error) },
            status: 'bad',
            statusKey: 'system.status.failed',
          }),
        );
        break;
      case 'off':
        rows.push(
          row({
            ...base,
            detail: b.latest
              ? { key: 'system.detail.backupLatest', values: { at } }
              : { key: 'system.detail.backupOff' },
            status: b.latest ? 'off' : 'warn',
            statusKey: 'system.status.off',
          }),
        );
        break;
      case 'ok':
        rows.push(
          row({
            ...base,
            detail: { key: 'system.detail.backupLatestNext', values: { at, next: b.next ?? '' } },
            status: 'ok',
          }),
        );
        break;
      case 'none':
        rows.push(
          row({
            ...base,
            detail: { key: 'system.detail.backupNone', values: { next: b.next ?? '' } },
            status: 'ok',
          }),
        );
    }
  }

  for (const lib of sys.libraries) {
    const low = !!lib.disk && lib.disk.total > 0 && lib.disk.free / lib.disk.total < LOW_DISK;
    rows.push(
      row({
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
      }),
    );
  }

  rows.push(
    row({
      kind: 'player',
      id: 'player',
      title: { key: 'system.row.player' },
      detail: { key: `system.detail.player.${sys.web_player || 'none'}` },
      status: sys.web_player ? 'ok' : 'off',
    }),
  );

  const u = sys.update;
  const available = u.update_available && u.latest;
  rows.push(
    row({
      kind: 'update',
      id: 'update',
      title: { key: 'system.row.update' },
      detail: !u.enabled
        ? { key: 'system.detail.updateOff' }
        : u.error
          ? { key: `about.error.${u.error}` }
          : available
            ? { key: 'system.detail.updateAvailable', values: { version: available.version } }
            : !u.checked_at
              ? { key: 'system.detail.notChecked' }
              : u.comparable
                ? { key: 'system.detail.upToDate' }
                : { key: 'system.detail.devBuild' },
      value: u.current,
      ...(!u.enabled
        ? { status: 'off' }
        : available
          ? { status: 'warn', statusKey: 'system.status.update' }
          : { status: u.error ? 'warn' : 'ok' }),
    }),
  );

  return rows;
}

/**
 * The local metadata copy in use: mirror mode (the server sends `mirror` only
 * then) with metadata on. Health > System's row and Settings' status line both
 * read the copy exactly when this does.
 */
export function activeMirror(sys: SystemStatus): MetaMirrorStatus | undefined {
  return sys.metadata.enabled ? sys.metadata.mirror : undefined;
}

/**
 * The community metadata row. In remote mode it is the service's health; in
 * mirror mode the local copy's state, which says more than the health check
 * (that goes to the copy once it is ready, to the service until then).
 */
function metadataRow(sys: SystemStatus): SystemRow {
  const m = sys.metadata;
  const base = {
    kind: 'metadata',
    id: 'metadata',
    title: { key: 'system.row.metadata' },
  } as const;
  const mirror = activeMirror(sys);
  if (mirror) {
    const look = mirrorLook(mirror);
    return row({
      ...base,
      detail: look.detail,
      status: look.status,
      statusKey: look.statusKey,
      mirror,
    });
  }
  const h = m.enabled && m.available ? m.health : null;
  return row({
    ...base,
    detail: !m.available
      ? { key: 'system.detail.metadataUnavailable' }
      : !h
        ? { key: 'system.detail.metadataOff' }
        : h.reachable
          ? { key: 'system.detail.metadataOk', values: { ms: h.latency_ms } }
          : { key: 'system.detail.metadataDown' },
    value: hostOf(m.base_url),
    status: !h ? 'off' : h.reachable ? 'ok' : 'bad',
  });
}

/**
 * The "service isn't responding" notice applies: the health check failed while
 * lookups go to the service. In mirror mode the check goes where lookups do (the
 * copy once it is ready, the service until then), so it counts only while the
 * copy isn't answering; with one that is, the copy's own panel says what's wrong.
 */
export function metadataDown(sys: SystemStatus): boolean {
  const h = sys.metadata.health;
  const mirror = activeMirror(sys);
  return !!h && !h.reachable && (!mirror || mirror.fallback);
}

/** One fact about the local copy: its label's i18n key and how the page formats the value. */
export interface MirrorFact {
  key: string;
  kind: 'mono' | 'number' | 'bytes' | 'date' | 'relative';
  value: string | number;
}

/**
 * How the local metadata copy reads (Health > System's row and panel, Settings'
 * status line): what follows from its status, beside the status itself.
 */
export interface MirrorLook {
  status: RowStatus;
  statusKey: string;
  /** The row's one-line detail. */
  detail: Text;
  /** Only the facts the server sent, in reading order. */
  facts: MirrorFact[];
  /** While downloading: compressed bytes, and the share done (undefined while the size isn't known). */
  progress: { done: number; total: number; fraction: number | undefined } | null;
  /** The last failure: its headline's i18n key and the server's sentence. */
  error: { key: string; text: string } | null;
  /** "Check now" makes sense (not while a download runs). */
  canCheck: boolean;
}

/**
 * Reads the local copy's status. The worst thing wins the row: a failed first
 * download, then no copy yet, a first download, a copy newer than this server,
 * a failed update, lookups going online, then an update downloading over a
 * working copy, then ready. A copy still answering is never shown as down.
 */
export function mirrorLook(m: MetaMirrorStatus): MirrorLook {
  const downloading = m.state === 'downloading';
  const schemaNewer = !!m.schema_newer;
  const error = m.error
    ? {
        key: m.state === 'error' ? 'system.mirror.failed' : 'system.mirror.updateFailed',
        text: m.error,
      }
    : null;

  let status: RowStatus;
  let statusKey: string | undefined;
  let detail: string;
  if (m.state === 'error') {
    [status, statusKey, detail] = ['bad', 'system.status.failed', 'mirrorFailed'];
  } else if (m.state === 'empty') {
    [status, statusKey, detail] = ['off', 'system.status.waiting', 'mirrorEmpty'];
  } else if (downloading && m.fallback) {
    [status, statusKey, detail] = ['off', 'system.status.downloading', 'mirrorFirstDownload'];
  } else if (schemaNewer) {
    [status, detail] = ['warn', 'mirrorNewer'];
  } else if (error) {
    [status, detail] = ['warn', 'mirrorUpdateFailed'];
  } else if (m.fallback) {
    [status, detail] = ['warn', 'mirrorFallback'];
  } else if (downloading) {
    [status, statusKey, detail] = ['ok', 'system.status.downloading', 'mirrorUpdating'];
  } else {
    [status, detail] = ['ok', 'mirrorReady'];
  }

  const facts: MirrorFact[] = [];
  const fact = (key: string, kind: MirrorFact['kind'], value: string | number | undefined) => {
    if (value !== undefined && value !== '')
      facts.push({ key: `system.mirror.fact.${key}`, kind, value });
  };
  fact('version', 'mono', m.tag);
  fact('built', 'date', m.built_at);
  fact('schema', 'number', m.schema_version);
  fact('size', 'bytes', m.size_bytes);
  fact('downloaded', 'relative', m.downloaded_at);
  fact('checked', 'relative', m.checked_at);
  fact('next', 'relative', m.next_check_at);

  const p = downloading ? m.progress : undefined;
  return {
    status,
    statusKey: statusKey ?? STATUS_WORD[status],
    detail: { key: `system.detail.${detail}` },
    facts,
    progress: p
      ? {
          done: p.done,
          total: p.total,
          fraction: p.total > 0 ? Math.min(1, p.done / p.total) : undefined,
        }
      : null,
    error,
    canCheck: !downloading,
  };
}

/** "0018_sessions.sql" -> 18. */
export function schemaNumber(name: string): number {
  return Number.parseInt(name, 10) || 0;
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
