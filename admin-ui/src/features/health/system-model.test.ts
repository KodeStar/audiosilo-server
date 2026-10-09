import { ApiError } from '@/api/client';
import { mirrorStatus, mirrorSystem, systemStatus, updateStatus } from '@/test/fixtures';
import {
  metadataDown,
  mirrorCheckRefusal,
  mirrorLook,
  schemaNumber,
  systemRows,
  uptime,
} from './system-model';

const NOW = Date.parse('2026-10-04T12:00:00Z');

const row = (rows: ReturnType<typeof systemRows>, id: string) => {
  const r = rows.find((x) => x.id === id);
  if (!r) throw new Error(`no row ${id}`);
  return r;
};

describe('systemRows', () => {
  it('reads a healthy server as healthy', () => {
    const rows = systemRows(systemStatus(), NOW);
    expect(rows.map((r) => r.id)).toEqual([
      'ffmpeg',
      'ffprobe',
      'metadata',
      'tls',
      'database',
      'library-1',
      'player',
      'update',
    ]);
    expect(rows.every((r) => r.status === 'ok' || r.status === 'off')).toBe(true);
    expect(row(rows, 'metadata').detail).toEqual({
      key: 'system.detail.metadataOk',
      values: { ms: 84 },
    });
  });

  it('flags a missing tool, an offline library and a full disk', () => {
    const rows = systemRows(
      systemStatus({
        tools: [
          { name: 'ffmpeg', path: '', version: '', source: '' },
          { name: 'ffprobe', path: '/data/tools/ffprobe', version: '7', source: 'downloaded' },
        ],
        libraries: [
          { id: 1, name: 'Fiction', root: '/a', available: false, disk: null },
          { id: 2, name: 'Kids', root: '/b', available: true, disk: { total: 100, free: 5 } },
        ],
      }),
      NOW,
    );
    expect(row(rows, 'ffmpeg').status).toBe('warn');
    expect(row(rows, 'ffprobe').detail.key).toBe('system.detail.ffprobeDownloaded');
    expect(row(rows, 'library-1')).toMatchObject({
      status: 'bad',
      detail: { key: 'system.detail.libraryOffline' },
    });
    expect(row(rows, 'library-2')).toMatchObject({
      status: 'warn',
      detail: { key: 'system.detail.diskLow' },
    });
  });

  it('never calls metadata a problem while it is off, and says when it is down', () => {
    const off = systemRows(
      systemStatus({
        metadata: {
          enabled: false,
          available: true,
          base_url: 'https://m.example',
          mode: 'remote',
          health: null,
        },
      }),
      NOW,
    );
    expect(row(off, 'metadata')).toMatchObject({ status: 'off', value: 'm.example' });
    const down = systemRows(
      systemStatus({
        metadata: {
          enabled: true,
          available: true,
          base_url: 'https://m.example',
          mode: 'remote',
          health: {
            reachable: false,
            latency_ms: 0,
            checked_at: '2026-10-04T11:59:00Z',
            error: 'not responding',
          },
        },
      }),
      NOW,
    );
    expect(row(down, 'metadata').status).toBe('bad');
  });

  it('reads certificates: valid, expiring, expired, not issued, plain HTTP', () => {
    const cert = (notAfter: string, issued = true) =>
      systemStatus({
        tls: {
          mode: 'autocert',
          hosts: ['b.example'],
          certificates: [
            {
              host: 'b.example',
              issued,
              subject: '',
              issuer: 'R11',
              not_before: '',
              not_after: notAfter,
              self_signed: false,
              dns_names: [],
            },
          ],
        },
      });
    expect(row(systemRows(cert('2026-12-04T12:00:00Z'), NOW), 'tls')).toMatchObject({
      status: 'ok',
      value: 'R11',
    });
    expect(row(systemRows(cert('2026-10-10T12:00:00Z'), NOW), 'tls')).toMatchObject({
      status: 'warn',
      detail: { key: 'settings.network.cert.expiring', values: { days: 6 } },
    });
    expect(row(systemRows(cert('2026-10-01T12:00:00Z'), NOW), 'tls').status).toBe('bad');
    expect(row(systemRows(cert('', false), NOW), 'tls').statusKey).toBe('system.status.waiting');
    const plain = systemStatus({ tls: { mode: 'off', hosts: [], certificates: [] } });
    expect(row(systemRows(plain, NOW), 'tls')).toMatchObject({
      status: 'off',
      detail: { key: 'system.detail.tlsOff' },
    });
  });

  it('shows an available update as a warning, never a failure', () => {
    const rows = systemRows(
      systemStatus({
        update: updateStatus({
          update_available: true,
          latest: { version: 'v1.16.0', name: '', url: 'https://github.com/x', published_at: '' },
        }),
      }),
      NOW,
    );
    expect(row(rows, 'update')).toMatchObject({
      status: 'warn',
      statusKey: 'system.status.update',
    });
  });
});

describe('the local metadata copy (mirror mode)', () => {
  it('reads a ready copy as healthy, with its facts in order', () => {
    const look = mirrorLook(mirrorStatus());
    expect(look).toMatchObject({
      state: 'ready',
      status: 'ok',
      statusKey: 'system.status.ok',
      detail: { key: 'system.detail.mirrorReady' },
      progress: null,
      error: null,
      fallback: false,
      schemaNewer: false,
      canCheck: true,
    });
    expect(look.facts.map((f) => [f.key, f.kind])).toEqual([
      ['system.mirror.fact.version', 'mono'],
      ['system.mirror.fact.built', 'date'],
      ['system.mirror.fact.schema', 'number'],
      ['system.mirror.fact.size', 'bytes'],
      ['system.mirror.fact.downloaded', 'relative'],
      ['system.mirror.fact.checked', 'relative'],
      ['system.mirror.fact.next', 'relative'],
    ]);
  });

  it('lists only the facts the server sent', () => {
    expect(mirrorLook({ state: 'empty', fallback: true }).facts).toEqual([]);
    expect(
      mirrorLook({ state: 'empty', fallback: true, checked_at: '2026-10-09T06:00:00Z' }).facts,
    ).toEqual([
      { key: 'system.mirror.fact.checked', kind: 'relative', value: '2026-10-09T06:00:00Z' },
    ]);
  });

  it('waits without a copy, then downloads the first one', () => {
    expect(mirrorLook({ state: 'empty', fallback: true })).toMatchObject({
      status: 'off',
      statusKey: 'system.status.waiting',
      detail: { key: 'system.detail.mirrorEmpty' },
      fallback: true,
      canCheck: true,
    });
    const first = mirrorLook({
      state: 'downloading',
      fallback: true,
      progress: { done: 110_000_000, total: 440_000_000 },
    });
    expect(first).toMatchObject({
      status: 'off',
      statusKey: 'system.status.downloading',
      detail: { key: 'system.detail.mirrorFirstDownload' },
      progress: { done: 110_000_000, total: 440_000_000, fraction: 0.25 },
      canCheck: false,
    });
  });

  it("shows a download's size as unknown until the server knows it", () => {
    expect(
      mirrorLook({ state: 'downloading', fallback: true, progress: { done: 5, total: 0 } })
        .progress,
    ).toEqual({ done: 5, total: 0, fraction: undefined });
    // Progress only counts while downloading.
    expect(mirrorLook(mirrorStatus({ progress: { done: 1, total: 2 } })).progress).toBeNull();
  });

  it('keeps a working copy healthy while an update downloads', () => {
    expect(
      mirrorLook(mirrorStatus({ state: 'downloading', progress: { done: 1, total: 4 } })),
    ).toMatchObject({
      status: 'ok',
      statusKey: 'system.status.downloading',
      detail: { key: 'system.detail.mirrorUpdating' },
      canCheck: false,
    });
  });

  it('fails only without a usable copy; a failed update over one needs attention', () => {
    expect(
      mirrorLook({ state: 'error', fallback: true, error: 'not enough disk space' }),
    ).toMatchObject({
      status: 'bad',
      statusKey: 'system.status.failed',
      detail: { key: 'system.detail.mirrorFailed' },
      error: { key: 'system.mirror.failed', text: 'not enough disk space' },
      canCheck: true,
    });
    expect(mirrorLook(mirrorStatus({ error: 'digest mismatch' }))).toMatchObject({
      status: 'warn',
      statusKey: 'system.status.attention',
      detail: { key: 'system.detail.mirrorUpdateFailed' },
      error: { key: 'system.mirror.updateFailed', text: 'digest mismatch' },
    });
  });

  it('flags a copy newer than this server, ahead of a failed update', () => {
    expect(
      mirrorLook(mirrorStatus({ schema_newer: true, schema_version: 8, error: 'x' })),
    ).toMatchObject({
      status: 'warn',
      detail: { key: 'system.detail.mirrorNewer' },
      schemaNewer: true,
      error: { key: 'system.mirror.updateFailed' },
    });
  });

  it('says when lookups go to the online service with a copy on disk', () => {
    expect(mirrorLook(mirrorStatus({ fallback: true }))).toMatchObject({
      status: 'warn',
      detail: { key: 'system.detail.mirrorFallback' },
      fallback: true,
    });
  });

  it('makes the metadata row the local copy in mirror mode', () => {
    const r = row(systemRows(mirrorSystem(), NOW), 'metadata');
    expect(r).toMatchObject({
      status: 'ok',
      value: '',
      detail: { key: 'system.detail.mirrorReady' },
      mirror: { state: 'ready' },
    });
    const failed = row(
      systemRows(mirrorSystem({ state: 'error', fallback: true, error: 'x' }), NOW),
      'metadata',
    );
    expect(failed).toMatchObject({ status: 'bad', statusKey: 'system.status.failed' });
  });

  it('reads mirror mode with metadata off as off, and remote mode as before', () => {
    const sys = mirrorSystem();
    const off = row(
      systemRows({ ...sys, metadata: { ...sys.metadata, enabled: false, health: null } }, NOW),
      'metadata',
    );
    expect(off).toMatchObject({ status: 'off', detail: { key: 'system.detail.metadataOff' } });
    expect(off.mirror).toBeUndefined();
    expect(row(systemRows(systemStatus(), NOW), 'metadata').mirror).toBeUndefined();
  });

  it('calls the service down only in remote mode', () => {
    const down = { reachable: false, latency_ms: 0, checked_at: '' };
    const remote = systemStatus();
    expect(metadataDown(remote)).toBe(false);
    expect(metadataDown({ ...remote, metadata: { ...remote.metadata, health: down } })).toBe(true);
    const mirror = mirrorSystem(mirrorStatus({ state: 'empty', fallback: true }));
    expect(metadataDown({ ...mirror, metadata: { ...mirror.metadata, health: down } })).toBe(false);
  });

  it('words a refused Check now', () => {
    expect(
      mirrorCheckRefusal(new ApiError(404, 'community metadata is turned off', 'metadata_off')),
    ).toBe('system.mirror.checkOff');
    expect(mirrorCheckRefusal(new ApiError(409, 'not in mirror mode', 'not_mirror_mode'))).toBe(
      'system.mirror.checkNotMirror',
    );
    // The bare {"error":"not_mirror_mode"} envelope reads the same.
    expect(mirrorCheckRefusal(new ApiError(409, 'not_mirror_mode'))).toBe(
      'system.mirror.checkNotMirror',
    );
    expect(mirrorCheckRefusal(new ApiError(500, 'boom'))).toBeUndefined();
    expect(mirrorCheckRefusal(new TypeError('Failed to fetch'))).toBeUndefined();
  });
});

describe('systemRows backups', () => {
  const latest = {
    name: 'audiosilo-20261004-030000Z-scheduled.db',
    size: 1,
    created_at: '2026-10-04T03:00:00Z',
    kind: 'scheduled' as const,
  };
  const base = {
    dir: '/data/backups',
    running: false,
    last: null,
    latest,
    next: '2026-10-05T03:00:00Z',
  };

  it('sits after the database, and is left out by a server without backups', () => {
    const ids = systemRows(systemStatus({ backups: base }), NOW).map((r) => r.id);
    expect(ids.indexOf('backups')).toBe(ids.indexOf('database') + 1);
    expect(systemRows(systemStatus(), NOW).some((r) => r.id === 'backups')).toBe(false);
  });

  it('reads the newest backup, a failure, and no schedule', () => {
    expect(row(systemRows(systemStatus({ backups: base }), NOW), 'backups')).toMatchObject({
      status: 'ok',
      detail: {
        key: 'system.detail.backupLatestNext',
        values: { at: latest.created_at, next: base.next },
      },
      value: '/data/backups',
    });
    const failed = {
      ...base,
      last: { at: 'x', ok: false, trigger: 'scheduled' as const, error: 'disk_full' },
    };
    expect(row(systemRows(systemStatus({ backups: failed }), NOW), 'backups')).toMatchObject({
      status: 'bad',
      statusKey: 'system.status.failed',
      detail: { key: 'backups.failure.disk_full' },
    });
    expect(
      row(
        systemRows(systemStatus({ backups: { ...base, latest: null, next: null } }), NOW),
        'backups',
      ),
    ).toMatchObject({ status: 'warn', detail: { key: 'system.detail.backupOff' } });
    expect(
      row(systemRows(systemStatus({ backups: { ...base, next: null } }), NOW), 'backups'),
    ).toMatchObject({
      status: 'off',
      detail: { key: 'system.detail.backupLatest' },
    });
  });
});

describe('systemRows wording', () => {
  it("says a development build isn't compared, and where the player comes from", () => {
    const rows = systemRows(
      systemStatus({
        web_player: 'embedded',
        update: updateStatus({ current: 'dev', comparable: false }),
      }),
      NOW,
    );
    expect(row(rows, 'update').detail.key).toBe('system.detail.devBuild');
    expect(row(rows, 'player').detail.key).toBe('system.detail.player.embedded');
  });
});

describe('helpers', () => {
  it('reads the schema number', () => {
    expect(schemaNumber('0018_sessions.sql')).toBe(18);
    expect(schemaNumber('')).toBe(0);
  });

  it('words uptime in whole units', () => {
    expect(uptime('2026-10-04T11:15:00Z', NOW)).toEqual([45, 'minute']);
    expect(uptime('2026-10-03T12:00:00Z', NOW)).toEqual([24, 'hour']);
    expect(uptime('2026-09-24T12:00:00Z', NOW)).toEqual([10, 'day']);
  });
});
