import { systemStatus, updateStatus } from '@/test/fixtures';
import { schemaNumber, systemRows, uptime } from './system-model';

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
        metadata: { enabled: false, available: true, base_url: 'https://m.example', health: null },
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
