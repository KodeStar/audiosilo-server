import type {
  Activity,
  AdminLibrary,
  AdminSettings,
  SystemStatus,
  UpdateStatus,
  AdminShare,
  AdminStats,
  Invite,
  InviteCreated,
  Device,
  IssuesSummary,
  ListeningSession,
  JobsState,
  ScanProgress,
  ServerInfo,
  User,
  UserDetail,
} from '@/api/types';

export const serverInfo: ServerInfo = {
  name: 'AudioSilo',
  server_id: 'srv-1',
  version: '0.9.2',
  api: 'v1',
  capabilities: {
    admin_ui: true,
    web_player: true,
    transcode: true,
    upload: false,
    websocket: false,
    api_keys: true,
    export: true,
    metadata: true,
  },
  auth: { methods: ['auth_code', 'password'] },
  demo: { enabled: false },
};

export const admin: User = {
  id: 1,
  username: 'chris',
  role: 'admin',
  disabled: false,
  has_password: true,
  has_recovery: false,
  is_demo: false,
};

export const member: User = { ...admin, id: 2, username: 'sam', role: 'user' };

export function stats(over: Partial<AdminStats> = {}): AdminStats {
  const now = Date.now();
  return {
    total_books: 3249,
    total_libraries: 2,
    total_users: 7,
    libraries: [
      { id: 1, name: 'Fiction', book_count: 2400 },
      { id: 2, name: 'Kids', book_count: 849 },
    ],
    listening: [
      {
        user_id: 2,
        username: 'sam',
        library_id: 1,
        path: 'Andy Weir/Project Hail Mary',
        title: 'Project Hail Mary',
        author: 'Andy Weir',
        position: 3000,
        duration: 6000,
        finished: false,
        updated_at: new Date(now - 60_000).toISOString(),
      },
      {
        user_id: 3,
        username: 'maya',
        library_id: 2,
        path: 'Peter Brown/The Wild Robot',
        title: 'The Wild Robot',
        author: 'Peter Brown',
        position: 100,
        duration: 100,
        finished: true,
        updated_at: new Date(now - 3 * 3600_000).toISOString(),
      },
    ],
    ...over,
  };
}

/** sam, live on an iPhone, halfway through Project Hail Mary (the stats fixture's row). */
export function liveSession(over: Partial<ListeningSession> = {}): ListeningSession {
  const now = Date.now();
  return {
    id: 41,
    user_id: 2,
    username: 'sam',
    library_id: 1,
    path: 'Andy Weir/Project Hail Mary',
    title: 'Project Hail Mary',
    author: 'Andy Weir',
    device_id: 7,
    device_name: "Sam's iPhone",
    client: { app: 'AudioSilo', version: '1.4.2', platform: 'ios' },
    started_at: new Date(now - 20 * 60_000).toISOString(),
    last_at: new Date(now - 10_000).toISOString(),
    start_position: 1800,
    position: 3000,
    duration: 6000,
    speed: 1,
    listened: 1200,
    codec: 'aac',
    transcoded: false,
    finished: false,
    backfilled: false,
    state: 'playing',
    chapter: 'Chapter 12',
    ip: '192.168.1.24',
    ...over,
  };
}

/** sam's paired iPhone. */
export function device(over: Partial<Device> = {}): Device {
  return {
    id: 7,
    user_id: 2,
    username: 'sam',
    kind: 'session',
    name: "Sam's iPhone",
    client: { app: 'AudioSilo', version: '1.4.2', platform: 'ios' },
    created_at: '2026-09-01T10:00:00Z',
    last_seen: new Date(Date.now() - 10_000).toISOString(),
    last_ip: '192.168.1.24',
    current: false,
    ...over,
  };
}

/** A 7-day Activity period ending 2026-10-04: sam listened most, one transcode, an old app build. */
export function activity(over: Partial<Activity> = {}): Activity {
  const H = 3600;
  const dates = [
    '2026-09-28',
    '2026-09-29',
    '2026-09-30',
    '2026-10-01',
    '2026-10-02',
    '2026-10-03',
    '2026-10-04',
  ];
  const grid = Array.from({ length: 7 }, () => new Array<number>(24).fill(0));
  grid[5][21] = 2 * H;
  return {
    range: '7d',
    estimated: 0,
    from: '2026-09-27T12:00:00Z',
    to: '2026-10-04T12:00:00Z',
    timezone: 'BST',
    utc_offset: 60,
    totals: { listened: 9 * H, sessions: 12, listeners: 2, books: 3, finished: 1 },
    previous: { listened: 6 * H, sessions: 12, listeners: 2, books: 2, finished: 0 },
    days: dates.map((date, i) => ({
      date,
      listened: i % 2 ? 2 * H : H / 2,
      by_user: [
        { user_id: 1, listened: i % 2 ? H : 0 },
        { user_id: 2, listened: i % 2 ? H : H / 2 },
      ],
    })),
    hour_weekday: grid,
    top_books: [
      {
        library_id: 1,
        path: 'Andy Weir/Project Hail Mary',
        title: 'Project Hail Mary',
        author: 'Andy Weir',
        listened: 5 * H,
        listeners: 2,
      },
    ],
    top_authors: [{ name: 'Andy Weir', listened: 5 * H, books: 1 }],
    top_narrators: [{ name: 'Ray Porter', listened: 5 * H, books: 1 }],
    top_users: [
      { user_id: 2, username: 'sam', listened: 5 * H, sessions: 8, books: 2, finished: 1 },
      { user_id: 1, username: 'chris', listened: 4 * H, sessions: 4, books: 1, finished: 0 },
    ],
    funnel: { started: 4, reached_25: 3, reached_50: 2, reached_75: 2, finished: 1 },
    drop_offs: [
      {
        library_id: 1,
        path: 'Terry Pratchett/Guards! Guards!',
        title: 'Guards! Guards!',
        chapter_index: 6,
        chapter: 'Part 7',
        listeners: 3,
        scan_error: true,
      },
    ],
    playback: [
      { transcoded: false, codec: 'aac', listened: 8 * H, sessions: 10 },
      { transcoded: true, codec: 'opus', listened: H, sessions: 2 },
    ],
    peak_concurrent: { streams: 3, at: '2026-10-03T20:00:00Z' },
    clients: [
      { app: 'AudioSilo', version: '1.4.2', platform: 'ios', devices: 2 },
      { app: 'AudioSilo', version: '1.3.0', platform: 'ios', devices: 1 },
    ],
    growth: [
      { date: '2026-09-28', books: 3200 },
      { date: '2026-10-04', books: 3249 },
    ],
    storage: {
      bytes: 2.6e12,
      by_library: [
        { library_id: 1, name: 'Fiction', bytes: 2.2e12, books: 2400 },
        { library_id: 2, name: 'Kids', bytes: 0.4e12, books: 849 },
      ],
      by_format: [{ key: 'm4b', bytes: 2.6e12, books: 3249 }],
      by_codec: [{ key: 'aac', bytes: 2.6e12, books: 3249 }],
    },
    coverage: { books: 3249, identified: 3000, with_chapters: 3100, with_cover: 3200 },
    inactive_users: [{ user_id: 5, username: 'priya', last_seen_at: '2026-07-01T10:00:00Z' }],
    ...over,
  };
}

export function settingsWith(over: Partial<AdminSettings> = {}): AdminSettings {
  return {
    general: { name: '', public_url: '', update_check: true, session_days: 400 },
    network: {
      bind: '0.0.0.0:8080',
      tls_mode: 'selfsigned',
      tls_hosts: [],
      trusted_proxies: [],
      cors_origins: [],
    },
    players: {
      web_dir: '/app/web',
      web_player: 'dir',
      apple_app_ids: [],
      android_package: '',
      android_sha256: [],
    },
    metadata: {
      enabled: true,
      base_url: 'https://meta.audiosilo.app',
      region: '',
      available: true,
    },
    demo: { enabled: false, library: '', max_users: null, max_users_default: 200, idle_ttl: '' },
    backups: { schedule: 'daily:03:00', keep: 7, dir: '' },
    locked: { 'players.web_dir': 'AUDIOSILO_WEB_DIR' },
    restart_settings: [
      'network.bind',
      'network.tls_mode',
      'network.tls_hosts',
      'players.web_dir',
      'metadata.base_url',
      'demo.enabled',
      'demo.idle_ttl',
      'backups.dir',
    ],
    restart_pending: [],
    ...over,
  };
}

export const settings: AdminSettings = settingsWith();

export function updateStatus(over: Partial<UpdateStatus> = {}): UpdateStatus {
  return {
    enabled: true,
    current: 'v1.15.0',
    latest: {
      version: 'v1.15.0',
      name: 'v1.15.0',
      url: 'https://github.com/KodeStar/audiosilo-server/releases/tag/v1.15.0',
      published_at: '2026-09-20T10:00:00Z',
    },
    update_available: false,
    comparable: true,
    checked_at: '2026-10-04T09:00:00Z',
    error: '',
    install: 'docker',
    ...over,
  };
}

export function systemStatus(over: Partial<SystemStatus> = {}): SystemStatus {
  return {
    name: 'Hearthside',
    server_id: 'srv-1',
    version: 'v1.15.0',
    go_version: 'go1.25.3',
    os: 'linux',
    arch: 'amd64',
    install: 'docker',
    started_at: '2026-10-01T09:00:00Z',
    data_dir: '/data',
    database: { bytes: 182_000_000, schema: '0018_sessions.sql' },
    tools: [
      { name: 'ffmpeg', path: '/usr/bin/ffmpeg', version: '6.1.1', source: 'local' },
      { name: 'ffprobe', path: '/usr/bin/ffprobe', version: '6.1.1', source: 'local' },
    ],
    metadata: {
      enabled: true,
      available: true,
      base_url: 'https://meta.audiosilo.app',
      health: { reachable: true, latency_ms: 84, checked_at: '2026-10-04T09:00:00Z' },
    },
    tls: {
      mode: 'selfsigned',
      hosts: [],
      certificates: [
        {
          host: '',
          issued: true,
          subject: 'AudioSilo',
          issuer: 'AudioSilo',
          not_before: '2026-01-01T00:00:00Z',
          not_after: '2036-01-01T00:00:00Z',
          self_signed: true,
          dns_names: ['localhost'],
        },
      ],
    },
    libraries: [
      {
        id: 1,
        name: 'Fiction',
        root: '/mnt/tank/fiction',
        available: true,
        disk: { total: 8e12, free: 5.4e12 },
      },
    ],
    web_player: 'dir',
    update: updateStatus(),
    ...over,
  };
}

/** No scan running. */
export const idle: ScanProgress = {
  running: false,
  total: 0,
  done: 0,
  indexed: 0,
  added: 0,
  updated: 0,
  moved: 0,
  removed: 0,
};

export function libraries(over: Partial<AdminLibrary>[] = []): AdminLibrary[] {
  const base: AdminLibrary[] = [
    {
      id: 1,
      name: 'Fiction',
      root: '/mnt/tank/fiction',
      default_view: '',
      sort_order: 0,
      book_count: 2400,
      available: true,
      scan: idle,
      scan_schedule: '',
      ignore_patterns: [],
      metadata_source: 'tags',
    },
    {
      id: 2,
      name: 'Kids',
      root: '/mnt/nas/kids',
      default_view: '',
      sort_order: 1,
      book_count: 849,
      available: true,
      scan: idle,
      scan_schedule: '',
      ignore_patterns: [],
      metadata_source: 'tags',
    },
  ];
  return base.map((l, i) => ({ ...l, ...over[i] }));
}

export const sam: User = {
  id: 2,
  username: 'sam',
  role: 'user',
  disabled: false,
  has_password: false,
  has_recovery: false,
  is_demo: false,
  last_seen_at: new Date(Date.now() - 3600_000).toISOString(),
};

export const users: User[] = [admin, sam];

export const kidsShare: AdminShare = {
  id: 7,
  name: 'Cosy mysteries',
  description: '',
  read_only: false,
  paths: [{ library_id: 1, path: 'Agatha Christie' }],
  member_ids: [2],
};

export const fictionGrant: AdminShare = {
  id: 8,
  name: 'Library: Fiction',
  description: 'Whole library',
  read_only: false,
  paths: [{ library_id: 1, path: '' }],
  whole_library_id: 1,
  member_ids: [],
};

export function invite(over: Partial<Invite> = {}): Invite {
  return {
    id: 31,
    label: 'invite',
    max_uses: 5,
    uses: 1,
    expires_at: new Date(Date.now() + 2 * 86400_000).toISOString(),
    redeemed_at: new Date(Date.now() - 3600_000).toISOString(),
    created_at: new Date(Date.now() - 5 * 86400_000).toISOString(),
    user_id: 2,
    username: 'sam',
    ...over,
  };
}

export function samDetail(over: Partial<UserDetail> = {}): UserDetail {
  return {
    user: sam,
    accessible_libraries: [],
    shares: [kidsShare],
    auth_codes: [invite()],
    ...over,
  };
}

export const created: InviteCreated = {
  auth_code: 'ABCD-1234',
  invite_url: 'https://books.example.com/connect#code=ABCD-1234',
  max_uses: 5,
  expires_at: new Date(Date.now() + 7 * 86400_000 + 60_000).toISOString(),
};

/** GET /admin/issues: a few books needing attention, nothing offline. */
export function issuesSummary(over: Partial<IssuesSummary> = {}): IssuesSummary {
  return {
    categories: [
      { kind: 'scan_error', count: 1, ignored: 0, samples: [] },
      { kind: 'suspect', count: 0, ignored: 0, samples: [] },
      { kind: 'duplicate', count: 1, ignored: 0, samples: [] },
      { kind: 'no_cover', count: 2, ignored: 1, samples: [] },
      { kind: 'no_chapters', count: 0, ignored: 0, samples: [] },
      { kind: 'transcode', count: 0, ignored: 0, samples: [] },
    ],
    offline: [],
    checked_at: '2026-10-04T08:00:00Z',
    ...over,
  };
}

/** GET /admin/jobs: nothing running, nothing queued, no schedules. */
export function jobsState(over: Partial<JobsState> = {}): JobsState {
  return { running: null, queued: [], schedules: [], ...over };
}
