// Hand-mirrored wire types for the server endpoints the console uses (no
// codegen, by design: see CROSS-REPO.md section 2). Mirror every field the
// server emits, even ones the console doesn't read yet, and keep the Go source
// named next to each type so a reviewer can check the pair.

/** GET /api/v1/server (handlers_auth.go handleServerInfo). Public. */
export interface ServerInfo {
  name: string;
  server_id: string;
  version: string;
  api: string;
  capabilities: {
    admin_ui: boolean;
    web_player: boolean;
    transcode: boolean;
    upload: boolean;
    websocket: boolean;
    api_keys: boolean;
    export: boolean;
    metadata: boolean;
  };
  auth: { methods: string[] };
  demo: { enabled: boolean };
}

/** auth.User (internal/auth/auth.go). Returned by /me and login. */
export interface User {
  id: number;
  username: string;
  role: 'admin' | 'user';
  disabled: boolean;
  has_password: boolean;
  has_recovery: boolean;
  is_demo: boolean;
  last_seen_at?: string;
}

/** POST /api/v1/auth/login (handlers_auth.go handleLogin). */
export interface LoginResponse {
  token: string;
  user: User;
  server_id: string;
}

/** One library's book count in GET /admin/stats (handlers_stats.go libStat). */
export interface LibraryStat {
  id: number;
  name: string;
  book_count: number;
}

/** catalog.ListeningRow (internal/catalog/listening.go). */
export interface ListeningRow {
  user_id: number;
  username: string;
  library_id: number;
  path: string;
  title: string;
  author: string;
  position: number;
  duration: number;
  finished: boolean;
  updated_at: string;
}

/** GET /api/v1/admin/stats (handlers_stats.go handleStats). */
export interface AdminStats {
  total_books: number;
  total_libraries: number;
  total_users: number;
  libraries: LibraryStat[];
  listening: ListeningRow[];
}

// Sessions, devices and listening stats (admin redesign Phase 4a:
// internal/catalog/{sessions,activity,progress_admin}.go, internal/auth/devices.go).

/**
 * The app behind a token or a session (auth.ClientInfo, catalog.Client), parsed
 * from the X-AudioSilo-Client header. `app` is "" for a client that never named
 * itself (released before the header).
 */
export interface ClientInfo {
  app: string;
  version: string;
  platform: string;
}

/** A session's state from the age of its newest save (catalog.Session*). */
export type SessionState = 'playing' | 'paused' | 'ended';

/** catalog.Session: one stretch of listening on one device, derived from progress saves. */
export interface ListeningSession {
  id: number;
  user_id: number;
  username: string;
  library_id: number;
  path: string;
  /** From the index; "" when the book isn't indexed (any more): the path names it. */
  title: string;
  author: string;
  /** The token's id (GET /admin/devices); name and client are copied at the time. */
  device_id: number;
  device_name: string;
  client: ClientInfo | null;
  started_at: string;
  last_at: string;
  start_position: number;
  position: number;
  duration: number;
  speed: number;
  /** Wall-clock seconds of playback. */
  listened: number;
  codec: string;
  transcoded: boolean;
  finished: boolean;
  /**
   * Not recorded live: made at the upgrade from the player's own listening
   * history (no device, app or playback mode), or imported (`imported`).
   */
  backfilled: boolean;
  /**
   * Imported from another server (Settings > Import): `client` names it (e.g.
   * Audiobookshelf) and the device is the one it recorded.
   */
  imported: boolean;
  state: SessionState;
  /**
   * Live sessions only: the chapter at the position and the device's newest address. The
   * chapter's title is omitted when it names nothing ("024", "Track 01"): `chapter_index`
   * places it (`chapterLabel`). A book with a single chapter has neither.
   */
  chapter?: string;
  chapter_index?: number;
  ip?: string;
}

/** What a session list is narrowed to (GET /admin/sessions). */
export interface SessionFilter {
  user_id?: number;
  library_id?: number;
  path?: string;
}

/**
 * GET /admin/sessions: newest first by when each session started (an imported
 * session is old but has a new id); `next_before` is the last session's id and
 * asks for the next page (null at the end).
 */
export interface SessionPage {
  sessions: ListeningSession[];
  next_before: number | null;
}

/** Where GET /admin/sessions continues: the last session's id and, so a gone one doesn't matter, its start. */
export interface SessionCursor {
  before?: number;
  before_at?: string;
}

/** auth.Device: a signed-in token (a paired phone, a browser) or a personal API key. */
export interface Device {
  id: number;
  user_id: number;
  username: string;
  kind: 'session' | 'api';
  /** The device name sent at sign-in (an API key's label). */
  name: string;
  /** null until the token makes a request naming its app. */
  client: ClientInfo | null;
  created_at: string;
  last_seen: string | null;
  /** The newest request's address ("" before any). */
  last_ip: string;
  /** The token making this request (the console itself): it can't be signed out here. */
  current: boolean;
}

/** catalog.UserProgress: one of a person's books with its start and finish dates. */
export interface UserProgress {
  library_id: number;
  path: string;
  position: number;
  duration: number;
  finished: boolean;
  playback_speed: number;
  version: number;
  device_id: string;
  updated_at: string;
  /** "" when the book isn't indexed (any more). */
  title: string;
  author: string;
  started_at: string | null;
  finished_at: string | null;
}

/**
 * PATCH /admin/libraries/{id}/progress?path=&user_id= (handleEditProgress). Absent
 * fields stay; a null date clears it; a date is RFC 3339 or YYYY-MM-DD (the start of
 * that day, server time).
 */
export interface ProgressEdit {
  finished?: boolean;
  position?: number;
  started_at?: string | null;
  finished_at?: string | null;
}

/** The Activity page's periods (catalog.ParseActivityRange); a year is also accepted. */
export const ACTIVITY_RANGES = ['7d', '30d', '90d', '1y'] as const;
export type ActivityRange = (typeof ACTIVITY_RANGES)[number];

/** catalog.ActivityTotals. `listened` is wall-clock seconds; `finished` counts books finished. */
export interface ActivityTotals {
  listened: number;
  sessions: number;
  listeners: number;
  books: number;
  finished: number;
}

/** catalog.UserSeconds: one listener's share of a day. */
export interface UserSeconds {
  user_id: number;
  listened: number;
}

/** catalog.ActivityDay: a day of the period (YYYY-MM-DD, server time), zeros included. */
export interface ActivityDay {
  date: string;
  listened: number;
  by_user: UserSeconds[];
}

/** catalog.TopBook */
export interface TopBook {
  library_id: number;
  path: string;
  title: string;
  author: string;
  listened: number;
  listeners: number;
}

/** catalog.TopPerson: an author or narrator (the whole field value). */
export interface TopPerson {
  name: string;
  listened: number;
  books: number;
}

/** catalog.TopUser */
export interface TopUser {
  user_id: number;
  username: string;
  listened: number;
  sessions: number;
  books: number;
  finished: number;
}

/** catalog.Funnel: people x books with a save in the period, by how far each got. */
export interface Funnel {
  started: number;
  reached_25: number;
  reached_50: number;
  reached_75: number;
  finished: number;
}

/** catalog.DropOff: a chapter where several people stopped the same book. */
export interface DropOff {
  library_id: number;
  path: string;
  title: string;
  chapter_index: number;
  chapter: string;
  listeners: number;
  /** The book has a read problem the Health page lists (often the reason). */
  scan_error: boolean;
}

/** catalog.PlaybackShare: listening direct or transcoded, per codec ("" = unknown). */
export interface PlaybackShare {
  transcoded: boolean;
  codec: string;
  listened: number;
  sessions: number;
}

/** catalog.Peak: the most sessions open at once (`at` null with none). */
export interface PeakConcurrent {
  streams: number;
  at: string | null;
}

/** catalog.ClientCount: devices that listened with one app build (`app` "" = never named). */
export interface ClientCount {
  app: string;
  version: string;
  platform: string;
  devices: number;
}

/** catalog.GrowthPoint: books indexed now that had appeared by `date`. */
export interface GrowthPoint {
  date: string;
  books: number;
}

/** catalog.StorageLibrary */
export interface StorageLibrary {
  library_id: number;
  name: string;
  bytes: number;
  books: number;
}

/** catalog.StorageGroup: a format's or codec's share (`key` "" = unknown). */
export interface StorageGroup {
  key: string;
  bytes: number;
  books: number;
}

/** catalog.Storage */
export interface Storage {
  bytes: number;
  by_library: StorageLibrary[];
  by_format: StorageGroup[];
  by_codec: StorageGroup[];
}

/** catalog.Coverage: books with an ASIN/ISBN, with chapters (more than one), with a cover. */
export interface Coverage {
  books: number;
  identified: number;
  with_chapters: number;
  with_cover: number;
}

/** catalog.InactiveUser: an enabled account with no activity for 60 days. */
export interface InactiveUser {
  user_id: number;
  username: string;
  last_seen_at: string | null;
}

/**
 * catalog.ListeningDays (GET /admin/listening): a period's listening day by day,
 * of everyone or one person, without the rest of the Activity page.
 */
export interface ListeningDays {
  range: string;
  from: string;
  to: string;
  timezone: string;
  /** The server's offset from UTC, in minutes. */
  utc_offset: number;
  days: ActivityDay[];
}

/** catalog.Activity: the Activity page for one period, bucketed in server time. */
export interface Activity {
  /** "7d", "30d", "90d", "1y" or a year ("2025"). */
  range: string;
  from: string;
  to: string;
  /** The server's zone abbreviation, and its offset from UTC in minutes. */
  timezone: string;
  utc_offset: number;
  totals: ActivityTotals;
  /** Seconds of totals.listened that are estimates (in the totals and tops, never in days). */
  estimated: number;
  /** The same length of time just before `from`, for the deltas. */
  previous: ActivityTotals;
  days: ActivityDay[];
  /** Listened seconds by weekday (0 = Monday) and hour, from raw sessions only. */
  hour_weekday: number[][];
  top_books: TopBook[];
  top_authors: TopPerson[];
  top_narrators: TopPerson[];
  top_users: TopUser[];
  funnel: Funnel;
  drop_offs: DropOff[];
  playback: PlaybackShare[];
  peak_concurrent: PeakConcurrent;
  clients: ClientCount[];
  growth: GrowthPoint[];
  storage: Storage;
  coverage: Coverage;
  inactive_users: InactiveUser[];
}

/** The HTTPS modes (config.TLSMode). */
export type TLSMode = 'off' | 'selfsigned' | 'autocert';

/**
 * GET/PATCH /api/v1/admin/settings (handlers_settings.go settingsEnvelope; each
 * section's fields come from internal/config/settings.go). A setting's id is
 * "<section>.<name>"; `locked`, `restart_settings` and `restart_pending` name ids.
 */
export interface AdminSettings {
  general: {
    /** "" = the default name, AudioSilo. */
    name: string;
    /** "" = derived from each request's host. */
    public_url: string;
    /** The home-network address; "" = derived from a request on a home-network host. */
    lan_url: string;
    update_check: boolean;
    /** Days raw listening sessions are kept before they become daily totals (30-3650). */
    session_days: number;
  };
  network: {
    bind: string;
    tls_mode: TLSMode;
    tls_hosts: string[];
    trusted_proxies: string[];
    cors_origins: string[];
  };
  players: {
    /** Read-only here: changed in config.yaml or AUDIOSILO_WEB_DIR. */
    web_dir: string;
    /** Where /web is served from (read-only). */
    web_player: '' | 'embedded' | 'dir';
    apple_app_ids: string[];
    android_package: string;
    android_sha256: string[];
  };
  metadata: {
    enabled: boolean;
    base_url: string;
    /** The Audible marketplace whose ASIN a match takes (one of MATCH_REGIONS; "" = the US store's). */
    region: string;
    /** The service exists (base_url was valid when the server started), so the switch can turn on. */
    available: boolean;
  };
  demo: {
    enabled: boolean;
    library: string;
    /** null = the default cap (max_users_default); 0 = no limit. */
    max_users: number | null;
    max_users_default: number;
    /** "" = 24h. */
    idle_ttl: string;
  };
  backups: {
    /** "" (off), "daily:HH:MM" or "weekly:DAY:HH:MM" in server time (backup.ParseSchedule). */
    schedule: string;
    /** How many scheduled backups are kept (1-365); manual ones stay until deleted. */
    keep: number;
    /** Read-only here: config.yaml or AUDIOSILO_BACKUP_DIR. "" = <data>/backups. */
    dir: string;
  };
  /** Setting id -> why the console can't change it: an AUDIOSILO_* variable, or "launcher". */
  locked: Record<string, string>;
  /** Setting ids read only at start. */
  restart_settings: string[];
  /** Restart settings saved with a value the running server doesn't use yet. */
  restart_pending: string[];
}

/** The sections of AdminSettings that hold settings. */
export type SettingsSection = 'general' | 'network' | 'players' | 'metadata' | 'demo' | 'backups';

/** A PATCH body: only the settings to change, by section. */
export type SettingsPatch = {
  [S in SettingsSection]?: Partial<AdminSettings[S]>;
};

/** server.Certificate (internal/server/certinfo.go). */
export interface Certificate {
  /** The host it was issued for (autocert), or "". */
  host: string;
  /** false: autocert hasn't obtained one yet. */
  issued: boolean;
  subject: string;
  issuer: string;
  not_before: string;
  not_after: string;
  self_signed: boolean;
  dns_names: string[];
}

/** api.Tool (handlers_system.go). */
export interface SystemTool {
  name: 'ffmpeg' | 'ffprobe';
  /** "" when off or not found. */
  path: string;
  version: string;
  source: '' | 'local' | 'downloaded';
}

/** meta.Health (internal/meta/health.go). */
export interface MetadataHealth {
  reachable: boolean;
  latency_ms: number;
  checked_at: string;
  error?: string;
}

/** GET /api/v1/admin/update and the update block of /admin/system (handlers_system.go updateStatus). */
export interface UpdateStatus {
  enabled: boolean;
  /** The running version ("dev" for a local build). */
  current: string;
  /** The newest release the last successful check found. */
  latest: { version: string; name: string; url: string; published_at: string } | null;
  update_available: boolean;
  /** false: the running version isn't a release, so it can't be compared. */
  comparable: boolean;
  checked_at: string | null;
  error: '' | 'rate_limited' | 'unreachable' | 'bad_response';
  install: 'docker' | 'binary' | 'source';
}

/** GET /api/v1/admin/system (handlers_system.go handleSystem). */
export interface SystemStatus {
  name: string;
  server_id: string;
  version: string;
  go_version: string;
  os: string;
  arch: string;
  install: UpdateStatus['install'];
  started_at: string;
  data_dir: string;
  database: { bytes: number; schema: string };
  tools: SystemTool[];
  metadata: {
    enabled: boolean;
    available: boolean;
    base_url: string;
    /** null while the lookup is off: nothing is asked. */
    health: MetadataHealth | null;
  };
  tls: { mode: TLSMode; hosts: string[]; certificates: Certificate[]; error?: string };
  libraries: {
    id: number;
    name: string;
    root: string;
    available: boolean;
    /** null when the root doesn't answer. */
    disk: { total: number; free: number } | null;
  }[];
  web_player: AdminSettings['players']['web_player'];
  update: UpdateStatus;
  /** Absent from a server before Phase 5b. */
  backups?: BackupStatus | null;
}

/** backup.Backup (internal/backup). */
export interface Backup {
  /** The file's name in the backups folder: also its id in the API. */
  name: string;
  size: number;
  created_at: string;
  kind: 'scheduled' | 'manual' | 'before-restore';
}

/** backup.Result: how the newest backup attempt went. */
export interface BackupResult {
  at: string;
  ok: boolean;
  trigger: 'scheduled' | 'manual';
  name?: string;
  /** "permission_denied", "disk_full" or "failed" ("" when ok). */
  error?: string;
}

/** backup.Status (GET /admin/backups' status, /admin/system's backups). */
export interface BackupStatus {
  dir: string;
  running: boolean;
  /** The newest attempt since the server started (null before one). */
  last: BackupResult | null;
  /** The newest backup in the folder, whenever it was made (not a copy made before a restore). */
  latest: Backup | null;
  /** The next scheduled backup; null while the schedule is off. */
  next: string | null;
}

/** backup.PendingRestore: a restore waiting for the next start. */
export interface PendingRestore {
  name: string;
  requested_at: string;
  requested_by: string;
  schema: string;
}

/** backup.RestoreResult: how the last restore went (written at start). */
export interface RestoreResult {
  name: string;
  applied_at: string;
  requested_by: string;
  ok: boolean;
  /** Why it wasn't applied: "missing", "unusable", "newer" or "failed". */
  error?: string;
  /** The copy kept of the database the restore replaced. */
  safety_copy?: string;
}

/** GET /api/v1/admin/backups (handlers_backups.go backupsEnvelope). */
export interface BackupsEnvelope {
  backups: Backup[];
  status: BackupStatus;
  restore: { pending: PendingRestore | null; last: RestoreResult | null };
}

/** Event kinds a destination can be sent (notify.Kinds, in the server's order). */
export type ServerEventKind =
  | 'book_added'
  | 'scan_failed'
  | 'library_unavailable'
  | 'new_device'
  | 'invite_redeemed'
  | 'update_available'
  | 'backup_failed';

export type NotifyTargetKind = 'webhook' | 'ntfy' | 'discord';

/**
 * A notification destination (handlers_notify.go notifyTarget). Its address and
 * secret never come back: `address` is redacted, `has_secret` says one is set.
 */
export interface NotifyTarget {
  id: number;
  kind: NotifyTargetKind;
  name: string;
  address: string;
  has_secret: boolean;
  enabled: boolean;
  events: ServerEventKind[];
  created_at: string;
  updated_at: string;
  last_at: string | null;
  last_ok: boolean | null;
  /** "timeout", "unreachable", "http_<status>" or "failed" ("" after a success). */
  last_error: string;
}

/** GET /api/v1/admin/notifications. */
export interface NotifyTargetsEnvelope {
  targets: NotifyTarget[];
  events: ServerEventKind[];
  kinds: NotifyTargetKind[];
}

/**
 * POST /admin/notifications (all but secret/enabled required) and PATCH
 * /admin/notifications/{id} (only what changes; no kind; secret "" clears it).
 */
export interface NotifyTargetInput {
  kind?: NotifyTargetKind;
  name?: string;
  url?: string;
  secret?: string;
  enabled?: boolean;
  events?: ServerEventKind[];
}

/** POST /admin/notifications/{id}/test. */
export interface NotifyTestResult {
  ok: boolean;
  error: string;
  target: NotifyTarget;
}

/** catalog.ServerEvent: one entry of the bell's feed. */
export interface ServerEvent {
  id: number;
  at: string;
  kind: ServerEventKind;
  /** The event's facts: library, count, titles, user, device, app, version, url, error... */
  data: Record<string, unknown>;
}

/** GET /api/v1/admin/events. */
export interface ServerEventPage {
  events: ServerEvent[];
  /** The next page's `before` (0 at the end). */
  next_before: number;
}

/** catalog.AuditEvent: one admin action. */
export interface AuditEvent {
  id: number;
  at: string;
  /** null for the server itself. */
  actor_id: number | null;
  actor_name: string;
  via: 'session' | 'api' | 'system';
  /** "<area>.<verb>", like "user.update". */
  action: string;
  target: string;
  details: Record<string, unknown>;
}

/** GET /api/v1/admin/audit. */
export interface AuditPage {
  events: AuditEvent[];
  next_before: number;
}

/** The audit log's filters (all optional). */
export interface AuditFilter {
  actor_id?: number;
  /** An action's first part: user, invite, library, book, share, settings, backup, notify, device, progress, issue. */
  area?: string;
  q?: string;
}

/** logring.Entry (internal/logring). */
export interface LogEntry {
  seq: number;
  time: string;
  level: 'debug' | 'info' | 'warn' | 'error';
  message: string;
  attrs: { key: string; value: string }[];
}

/** GET /api/v1/admin/logs. */
export interface LogPage {
  entries: LogEntry[];
  /** The newest line's seq: the next poll's `after`. */
  last_seq: number;
  /** Matching lines were left out (past the limit, or dropped before the cursor). */
  truncated: boolean;
}

/** catalog.Library (internal/catalog/model.go). */
export interface Library {
  id: number;
  name: string;
  root: string;
  default_view: string;
  sort_order: number;
}

/**
 * One library in GET /admin/libraries and PUT /admin/libraries/order
 * (handlers_admin.go adminLibrary): the library plus its indexed book count,
 * whether its root folder is reachable right now, and its scan progress.
 */
export interface AdminLibrary extends Library {
  book_count: number;
  available: boolean;
  scan: ScanProgress;
  /** "" = no scheduled scans; see SCAN_SCHEDULES (library.ParseSchedule). */
  scan_schedule: string;
  /** One pattern per entry (library.ParseIgnore); comments start with #. */
  ignore_patterns: string[];
  /** Where the books' title, author, series and position come from first. */
  metadata_source: MetadataSource;
  /** When the next scheduled scan is due (RFC 3339); absent without a schedule. */
  next_scan_at?: string;
}

/**
 * catalog.MetadataFromTags / MetadataFromPath: a library's books take their
 * details from the files' tags first, or from the folder layout first
 * (Author/Series/01 - Title); the other fills what the first leaves empty.
 */
export const METADATA_SOURCES = ['tags', 'path'] as const;
export type MetadataSource = (typeof METADATA_SOURCES)[number];

/** The body of POST /admin/libraries and PATCH /admin/libraries/{id} (handlers_admin.go libraryRequest). */
export interface LibraryRequest {
  name?: string;
  root?: string;
  scan_schedule?: string;
  ignore_patterns?: string[];
  metadata_source?: MetadataSource;
}

/** The `every:` intervals a library schedule may use (library.ParseSchedule). */
export const SCAN_INTERVALS = [
  'every:1h',
  'every:3h',
  'every:6h',
  'every:12h',
  'every:24h',
] as const;

/** library.ScanProgress (internal/library/scanner.go), GET /admin/libraries/{id}/scan. */
export interface ScanProgress {
  running: boolean;
  /** A scan of the library waits in the job queue (behind another, or to run again). */
  queued?: boolean;
  total: number;
  done: number;
  indexed: number;
  /** What the scan has changed so far. */
  added: number;
  updated: number;
  moved: number;
  removed: number;
  /** The last finished scan stopped at the unavailable-root guard (nothing pruned). */
  unavailable?: boolean;
}

/** Why a scan was queued (library.Trigger*). */
export type ScanTrigger = 'manual' | 'schedule' | 'startup' | 'change';

/** library.Job (internal/library/jobs.go): a queued or running scan. */
export interface Job {
  id: number;
  kind: 'scan';
  library_id: number;
  library_name: string;
  trigger: ScanTrigger;
  /** The admin who asked; null for a schedule or startup. */
  started_by: number | null;
  queued_at: string;
  started_at?: string;
  run_id?: number;
  /** A running job's progress. */
  progress?: ScanProgress;
}

/** One scheduled library in GET /admin/jobs (handlers_health.go scheduledScan). */
export interface ScheduledScan {
  library_id: number;
  library_name: string;
  schedule: string;
  next_at: string;
}

/** GET /admin/jobs (handlers_health.go handleJobs). */
export interface JobsState {
  running: Job | null;
  queued: Job[];
  schedules: ScheduledScan[];
}

/** catalog.RunRunning etc. (internal/catalog/scanruns.go). */
export type ScanRunStatus =
  'running' | 'ok' | 'partial' | 'unavailable' | 'failed' | 'cancelled' | 'interrupted';

/** catalog.RunEvent: one line of a scan's log (`kind` is a code the console words). */
export interface RunEvent {
  at: string;
  level: 'info' | 'warn' | 'error';
  kind: string;
  path?: string;
  to?: string;
  /** A read problem's code (ScanErrorCode). */
  code?: string;
  /** A tool's or the OS's own message, shown as is. */
  detail?: string;
  count?: number;
}

/** catalog.ScanRun: one recorded scan (GET /admin/scan-runs, /admin/scan-runs/{id}). */
export interface ScanRun {
  id: number;
  library_id: number;
  library_name: string;
  trigger: ScanTrigger;
  started_by: number | null;
  started_by_name?: string;
  started_at: string;
  finished_at: string | null;
  status: ScanRunStatus;
  books: number;
  added: number;
  updated: number;
  moved: number;
  removed: number;
  errors: number;
  /** Only on a single run. */
  log?: RunEvent[];
}

/** GET /admin/scan-runs. */
export interface ScanRunPage {
  runs: ScanRun[];
  next_before?: number;
}

/** catalog.IssueKinds, in the Health page's order. */
export const ISSUE_KINDS = [
  'scan_error',
  'suspect',
  'split_discs',
  'duplicate',
  'no_cover',
  'unmatched',
  'no_chapters',
  'detailed_chapters',
  'transcode',
] as const;
export type IssueKind = (typeof ISSUE_KINDS)[number];

/** A read problem's code (books.scan_error, library.noteProblem). */
export type ScanErrorCode = 'unreadable' | 'empty_file' | 'probe_failed';

/** catalog.IssueSample: a book shown on a category card. */
export interface IssueSample {
  library_id: number;
  path: string;
  title: string;
}

/** catalog.IssueCount: one category (for duplicates, counted in groups). */
export interface IssueCount {
  kind: IssueKind;
  count: number;
  ignored: number;
  samples: IssueSample[];
}

/** handlers_health.go offlineLibrary: a library whose folder can't be read, and what it keeps. */
export interface OfflineLibrary {
  library_id: number;
  name: string;
  root: string;
  books: number;
  listeners: number;
}

/** GET /admin/issues (handlers_health.go handleIssues). */
export interface IssuesSummary {
  categories: IssueCount[];
  offline: OfflineLibrary[];
  /** When a scan last finished ("" = never). */
  checked_at: string;
}

/** catalog.DuplicateMember. */
export interface DuplicateMember extends AdminBook {
  listeners: number;
}

/** catalog.DuplicateGroup: copies of one book in one library; books[0] is the one to keep. */
export interface DuplicateGroup {
  reason: 'same_files' | 'same_book';
  ignored: boolean;
  books: DuplicateMember[];
}

/** library.DirListing (internal/library/dirs.go), GET /admin/fs/dirs. */
export interface DirListing {
  path: string;
  parent?: string;
  dirs: { name: string; path: string }[];
  truncated?: boolean;
}

/** library.Entry (internal/library/fsview.go): one row of GET /libraries/{id}/fs. */
export interface FsEntry {
  name: string;
  path: string;
  is_dir: boolean;
  is_audio: boolean;
  size: number;
  mod_time: number;
  is_book?: boolean;
  title?: string;
  author?: string;
  series?: string;
  series_index?: number;
  duration?: number;
  override?: FolderMode;
  /**
   * A folder whose audio is only in disc folders directly in it (CD1, CD2...), each
   * read as its own book: "Always one book" joins them (library.discSets).
   */
  split_discs?: boolean;
}

/** library.Listing (internal/library/fsview.go). */
export interface FsListing {
  path: string;
  entries: FsEntry[];
  total: number;
  offset: number;
  next_offset?: number;
}

/** A folder-detection override (catalog.SetFolderOverride). */
export type FolderMode = 'book' | 'collection';

/** metadata.Chapter (internal/metadata/metadata.go). */
export interface Chapter {
  index: number;
  title: string;
  file_index: number;
  file_path: string;
  start: number;
  end: number;
  book_offset: number;
}

/** catalog.BookFile (internal/catalog/model.go). */
export interface BookFile {
  rel_path: string;
  seq: number;
  duration: number;
  format: string;
  size: number;
}

/** catalog.BookLocation (internal/catalog/model.go). */
export interface BookLocation {
  library_id: number;
  library_name: string;
  path: string;
  format?: string;
  size?: number;
  multi_file?: boolean;
}

/** catalog.Book (internal/catalog/model.go). */
export interface Book {
  id: number;
  library_id: number;
  rel_path: string;
  is_folder: boolean;
  title: string;
  author: string;
  series: string;
  series_index: number;
  narrator: string;
  duration: number;
  asin?: string;
  isbn?: string;
  format: string;
  codec?: string;
  size: number;
  added_at?: string;
  files?: BookFile[];
  chapters?: Chapter[];
  direct_playable?: boolean;
  dedup_key?: string;
  multi_file?: boolean;
  other_locations?: BookLocation[];
}

/** catalog.Page (internal/catalog/books.go), GET /libraries/{id}/books. */
export interface BookPage {
  books: Book[];
  next_cursor?: string;
}

/** auth.AuthCode (internal/auth/auth.go): an invite's metadata, never the code. */
export interface AuthCode {
  id: number;
  label: string;
  /** 0 = unlimited. */
  max_uses: number;
  uses: number;
  /** Empty or absent = never expires. */
  expires_at?: string;
  /** When the first device paired with it; absent = none yet. */
  redeemed_at?: string;
  created_at: string;
}

/** auth.Invite (internal/auth/auth.go): GET /admin/invites. */
export interface Invite extends AuthCode {
  user_id: number;
  username: string;
}

/**
 * POST /admin/users/{id}/authcode and POST /admin/authcodes/{id}/rotate
 * (handlers_admin.go): the code and its link, returned this once.
 */
export interface InviteCreated {
  auth_code: string;
  invite_url: string;
  /** 0 = unlimited. */
  max_uses: number;
  /** Absent = never expires. */
  expires_at?: string;
}

/** catalog.PathRule (internal/catalog/shares.go). An empty path is the whole library. */
export interface PathRule {
  library_id: number;
  path: string;
}

/** catalog.Share (internal/catalog/shares.go). */
export interface Share {
  id: number;
  name: string;
  description: string;
  read_only: boolean;
  paths?: PathRule[];
  /** Set on the share a whole-library grant makes: the library it grants. */
  whole_library_id?: number;
}

/** One share in GET /admin/shares (handlers_shares.go adminShare). */
export interface AdminShare extends Share {
  member_ids: number[];
}

/** GET /admin/users/{id} (handlers_admin.go handleGetUserDetail). */
export interface UserDetail {
  user: User;
  accessible_libraries: Library[] | null;
  shares: Share[] | null;
  auth_codes: AuthCode[] | null;
}

/** The {"error": "..."} envelope every handler uses for failures (respond.go). */
export interface ErrorEnvelope {
  error: string;
  /** For failures a person can fix (respond.go codeUsernameTaken etc.). */
  code?: string;
  /** With code "invalid_override": the book field the edit was refused for. */
  field?: string;
  /** Which rule a refused field broke (invalid_target: notify.Reason*), and a length's limit. */
  reason?: string;
  max?: number;
}

// ---- Admin catalog (Phase 2a API, consumed by the Library and Book screens) ----

/**
 * A cover's colours (catalog.CoverColor): `bg` its dominant colour, `accent` its most
 * vibrant one nudged to 4.5:1 against `bg` (absent when the art has none), `on_accent`
 * the type colour on the accent. Each lowercase `#rrggbb`.
 */
export interface CoverColor {
  bg: string;
  accent?: string;
  on_accent?: string;
}

/** One row of GET /admin/books (catalog.AdminBook, internal/catalog/adminbooks.go). */
export interface AdminBook {
  library_id: number;
  library_name: string;
  path: string;
  is_folder: boolean;
  title: string;
  author: string;
  narrator: string;
  /**
   * The people `author` and `narrator` name (catalog names.Split: "Michael
   * Kramer, Kate Reading" is two), each a value the author= / narrator= filters
   * find the book by.
   */
  authors: string[];
  narrators: string[];
  series: string;
  series_index: number;
  /** Every series the book is in, its main one (`series`) first, with its position in each. */
  series_list: SeriesRef[];
  published: string;
  /** Seconds. */
  duration: number;
  format: string;
  codec: string;
  direct_playable: boolean;
  size: number;
  added_at: string;
  has_cover: boolean;
  custom_cover: boolean;
  /** Colours read from the cover art. Absent until the server has read the current art's colour. */
  cover_color?: CoverColor;
  chapter_count: number;
  file_count: number;
  asin: string;
  isbn: string;
  /** An ASIN or ISBN is set: the community metadata can match it (the `matched=` filter's rule). */
  matched: boolean;
  edited: boolean;
  /** The fields with an override (an edit or an accepted community value). */
  edited_fields: OverrideField[];
  /** A read problem on its last indexing, in which file (library-relative), and the tool's message. */
  scan_error?: ScanErrorCode;
  scan_error_file?: string;
  scan_error_detail?: string;
  /** How many books its parts look like (>= 2: the folder may hold several). */
  suspect_parts?: number;
  /** Where its chapters come from now. */
  chapters_source: ChapterSource;
  /** Its last community chapter check's status (absent: never checked). */
  chapters_check?: CommunityChaptersStatus;
}

/** GET /admin/books (catalog.AdminPage). */
export interface AdminBookPage {
  books: AdminBook[];
  next_cursor?: string;
}

/** The orderings GET /admin/books accepts (catalog.adminSorts), in the sort menu's order. */
export const ADMIN_BOOK_SORTS = [
  'title',
  'author',
  'surname',
  'series',
  'narrator',
  'published',
  'added',
  'duration',
  'size',
] as const;
export type AdminBookSort = (typeof ADMIN_BOOK_SORTS)[number];

/** catalog.FacetCount. */
export interface FacetCount {
  value: string;
  count: number;
}

/** catalog.BoolFacet: books for which a yes/no property holds, and doesn't. */
export interface BoolFacet {
  yes: number;
  no: number;
}

/** GET /admin/books/facets (catalog.BookFacets): each dimension counted without its own filter. */
export interface BookFacets {
  total: number;
  libraries: { library_id: number; count: number }[];
  formats: FacetCount[];
  codecs: FacetCount[];
  direct_playable: BoolFacet;
  has_cover: BoolFacet;
  has_chapters: BoolFacet;
  matched: BoolFacet;
  edited: BoolFacet;
}

/**
 * catalog.PersonCount: one author or narrator, counting every book whose credit
 * names them ("Michael Kramer, Kate Reading" counts for both).
 */
export interface PersonCount {
  name: string;
  books: number;
  /** Seconds, summed over their books. */
  duration: number;
}

/** catalog.MergeSuggestion: spellings that look like one person. */
export interface MergeSuggestion {
  names: string[];
  suggested: string;
  books: number;
  /** The books a merge rewrites: those carrying one of the other spellings. */
  other_books: number;
}

/** GET /admin/authors (handlers_catalog.go handleAdminPeople). */
export interface AuthorsResponse {
  authors: PersonCount[];
  merge_suggestions: MergeSuggestion[];
  /** Books with no author. */
  unknown: number;
}

/** GET /admin/narrators (handlers_catalog.go handleAdminPeople). */
export interface NarratorsResponse {
  narrators: PersonCount[];
  merge_suggestions: MergeSuggestion[];
  unknown: number;
}

/** The two people aggregates (each person a credit names is one; see PersonCount). */
export type PersonField = 'author' | 'narrator';

/** Either people aggregate as the console reads it (api.people; not a wire shape). */
export interface PeopleResponse {
  people: PersonCount[];
  merge_suggestions: MergeSuggestion[];
  unknown: number;
}

/** catalog.SeriesCount (GET /admin/series). */
export interface SeriesCount {
  name: string;
  /** The most common author among its books. */
  author: string;
  books: number;
  duration: number;
  /** Distinct non-zero positions held, ascending. */
  positions: number[];
  /** How many of `books` are in it beyond their main series (catalog more_series). */
  extra_books: number;
}

/** One series a book is in, with its position there (0 = none): catalog.SeriesRef. */
export interface SeriesRef {
  name: string;
  position: number;
}

/** The overridable book fields (catalog.OverrideFields), in display order. */
export const OVERRIDE_FIELDS = [
  'title',
  'author',
  'narrator',
  'series',
  'series_index',
  'more_series',
  'published',
  'description',
  'asin',
  'isbn',
] as const;
export type OverrideField = (typeof OVERRIDE_FIELDS)[number];

/** Where a field's value came from (catalog.Source*), in the legend's order. */
export const FIELD_SOURCES = ['tag', 'path', 'edited', 'community'] as const;
/** A field's source; "" = no value. */
export type FieldSource = (typeof FIELD_SOURCES)[number] | '';

/** catalog.FieldValue: one overridable field on the book page. */
export interface FieldValue {
  value: string;
  source: FieldSource;
  /** What the scan found: the value a revert restores. */
  scanned: string;
  locked: boolean;
  edited_by?: string;
  edited_at?: string;
}

/** catalog.AdminChapter. Times are seconds. */
export interface AdminChapter {
  index: number;
  title: string;
  scanned_title: string;
  edited: boolean;
  file_path: string;
  start: number;
  end: number;
  book_offset: number;
}

/** catalog.AdminFile: one audio file of a book (bitrate in bits per second, 0 = unknown). */
export interface AdminFile {
  path: string;
  seq: number;
  duration: number;
  format: string;
  codec: string;
  size: number;
  bitrate: number;
}

/** catalog.Listener: one user's progress on the book. */
export interface Listener {
  user_id: number;
  username: string;
  position: number;
  duration: number;
  finished: boolean;
  updated_at: string;
  started_at: string | null;
  finished_at: string | null;
}

/** catalog.BookShare: a share that includes the book, by the rule that includes it. */
export interface BookShare {
  share_id: number;
  name: string;
  /** The granting rule ("" = whole library). */
  path: string;
  whole_library_id?: number;
}

/** GET/PATCH /admin/libraries/{id}/book (catalog.AdminBookDetail). */
export interface AdminBookDetail {
  book: AdminBook;
  description: string;
  fields: Record<OverrideField, FieldValue>;
  chapters: AdminChapter[];
  files: AdminFile[];
  listeners: Listener[];
  shares: BookShare[];
  /** The folder whose detection decides the book's shape, and its override ("" = automatic). */
  folder: { path: string; override: FolderMode | '' };
  indexed_at: string;
  /** Where `chapters` come from now, and the admin's choice of it ("" = automatic). */
  chapter_source: ChapterSource;
  chapter_choice: ChapterSource | '';
  /** The last community chapter check (null: never checked), and whether one is running. */
  community_chapters: CommunityChapters | null;
  community_checking: boolean;
  /** The last check failed (the community service, or it ran out of time): `community_chapters` is the one before. */
  community_check_failed?: boolean;
  /**
   * What the match dialog's search box opens with: the title and author, or the folders' when
   * the tags look swapped or junk (meta.SearchPrefill).
   */
  match_query: string;
}

/** Where a book's chapters come from: its own files, or a community list fitted onto them. */
export type ChapterSource = 'files' | 'community';

/**
 * A community chapter check's outcome (catalog.CommunityChapters, chapteralign.Status):
 * the fitted ones carry chapters, the rest say why not.
 */
export type CommunityChaptersStatus =
  | 'fill'
  | 'titles'
  | 'refine'
  | 'restructure'
  | 'same'
  | 'length_mismatch'
  | 'structure_mismatch'
  | 'crosses_files'
  | 'no_match'
  | 'unavailable';

/** The statuses that carry chapters. */
export const FITTED_STATUSES: readonly CommunityChaptersStatus[] = [
  'fill',
  'titles',
  'refine',
  'restructure',
  'same',
];

/** GET /admin/libraries/{id}/book's community_chapters (catalog.CommunityChapters). */
export interface CommunityChapters {
  status: CommunityChaptersStatus;
  work_id?: string;
  recording_id?: string;
  detail?: CommunityChaptersDetail;
  checked_at: string;
  /** The book changed since (other audio or identifiers): not used until checked again. */
  stale?: boolean;
}

/** What a check found (chapteralign.Detail). Seconds. */
export interface CommunityChaptersDetail {
  local_duration: number;
  community_duration: number;
  ratio?: number;
  worst?: number;
  local_chapters: number;
  community_chapters: number;
  anchors: number;
  snapped: number;
  approximate: number;
  /** Community chapters this copy lacks (a preview, end credits). */
  omitted?: string[];
  /** The book's chapters (by index) the community titles differently. */
  title_diffs?: { index: number; current: string; community: string }[];
  /** The community chapter a file boundary falls inside. */
  straddle?: { title: string; at: number; from: string; to: string };
}

/** The body of PATCH /admin/libraries/{id}/book (handlers_catalog.go editRequest). */
export interface BookEditRequest {
  set?: Partial<Record<OverrideField, string>>;
  revert?: OverrideField[];
  /** "community" when the values were accepted from a match; default "edited". */
  source?: 'edited' | 'community';
  chapters?: { set?: Record<number, string>; revert?: number[] };
  /** Where the chapters come from: the files, the community's, or "auto" (the default). */
  chapter_source?: ChapterSource | 'auto';
}

/** catalog.Ref: a book by its identity. */
export interface BookRef {
  library_id: number;
  path: string;
}

/** meta.MetaPersonRef. */
export interface MetaPersonRef {
  id: string;
  name: string;
}

/** meta.MatchRecording: one narration/edition of a candidate work. */
export interface MatchRecording {
  id: string;
  narrators: MetaPersonRef[];
  abridged?: boolean;
  runtime_min?: number;
  release_date?: string;
  publisher?: string;
  /** Best first for this server: the preferred marketplace's, then the US store's, then the rest. */
  asins: string[];
  /** Each ASIN with its marketplace, as the community lists them (absent from an older server). */
  asin_refs?: ASINRef[];
  /** The marketplace asins[0] sells in. */
  asin_region?: string;
  isbns: string[];
  cover_url?: string;
}

/** meta.ASINRef: one of a recording's ASINs and the Audible marketplace it sells in. */
export interface ASINRef {
  region: string;
  asin: string;
}

/** The Audible marketplaces (config.Regions, the community metadata's region vocabulary). */
export const MATCH_REGIONS = [
  'us',
  'uk',
  'ca',
  'au',
  'de',
  'fr',
  'es',
  'it',
  'jp',
  'in',
  'br',
] as const;

/** meta.MatchCandidate (GET /admin/libraries/{id}/book/match). */
export interface MatchCandidate {
  work_id: string;
  title: string;
  subtitle?: string;
  authors: MetaPersonRef[];
  language?: string;
  first_published?: string;
  description?: string;
  series: { name: string; position: string }[];
  cover_url?: string;
  web_url: string;
  recordings: MatchRecording[];
  /** The recording an ASIN/ISBN lookup resolved to. */
  recording_id?: string;
  /** The recording the book most likely is (the server's meta.DefaultRecording). */
  default_recording_id?: string;
  /**
   * 0-100: how well the work fits the book - the community service's structured
   * match score, 100 for an identifier hit, or (older service) the title, author
   * and runtime agreement.
   */
  score: number;
  /** Which facts agreed, when the community service's match scored it. */
  reasons?: MatchReasons;
}

/** meta.MatchReasons: each field present only when the request let it be judged. */
export interface MatchReasons {
  /** Best title similarity, 0-1. */
  title?: number;
  /** Typed-text similarity, 0-1 (1 when it named the series and volume). */
  text?: number;
  author?: 'full' | 'surname' | 'none';
  series?: 'position' | 'name' | 'conflict' | 'none';
  /** Relative runtime difference of the closest recording (0.02 = 2%). */
  runtime?: number;
  identifier?: 'asin' | 'isbn';
}

/** One entry of POST /admin/covers (handlers_covers.go coverThumb), in request order. */
export interface CoverThumb {
  library_id: number;
  path: string;
  /** A data: URL of a JPEG thumbnail, or "" when the book has no art. */
  data: string;
}

/** One entry of POST /admin/books/works (handlers_catalog.go bookWork), in request order. */
export interface BookWork {
  library_id: number;
  path: string;
  /** The community work the book's ASIN/ISBN resolves to; "" when it has none, has no match or the lookup failed. */
  work_id: string;
  /** This book's lookup failed or ran out of time: asking again later may resolve it. Never for a clean miss. */
  failed: boolean;
}

/** POST /admin/books/works: which community work each book is. */
export interface BookWorks {
  works: BookWork[];
}

/** meta.MetaPosition. */
export interface MetaPosition {
  chapter: number;
}

/** meta.MetaWork (only in the meta envelope here; the console reads its series rails). */
export interface MetaWork {
  id: string;
  title: string;
  subtitle?: string;
  authors: MetaPersonRef[];
  language: string;
  first_published?: string;
  description?: string;
  characters?: {
    id: string;
    name: string;
    aliases?: string[];
    role?: string;
    reveal: MetaPosition;
    description?: string;
  }[];
  recaps?: { through: MetaPosition; scope?: string; text: string }[];
  recap_summary?: { in_short?: string; ending?: string };
}

/** meta.MetaRecording. */
export interface MetaRecording {
  id: string;
  narrators: MetaPersonRef[];
  abridged?: boolean;
  runtime_min?: number;
  release_date?: string;
  publisher?: string;
  cover_url?: string;
}

/** meta.MetaSeriesWork: one entry of a series rail. */
export interface MetaSeriesWork {
  id: string;
  title: string;
  position: string;
  authors: MetaPersonRef[];
  cover_url?: string;
  web_url: string;
}

/** meta.MetaSeriesOrdering: an alternate reading order of a rail's family. */
export interface MetaSeriesOrdering {
  id: string;
  name: string;
  ordering?: string;
  ordering_of?: string;
  position?: string;
  works: MetaSeriesWork[];
}

/** meta.MetaSeries: a full ordered series rail (the family's main view). */
export interface MetaSeries {
  id: string;
  name: string;
  position: string;
  works: MetaSeriesWork[];
  ordering?: string;
  ordering_of?: string;
  orderings?: MetaSeriesOrdering[];
}

/**
 * GET /libraries/{id}/meta?path= (handlers_meta.go): {"matched": false}, or the
 * composed meta.Enrichment.
 */
export interface BookMeta {
  matched: boolean;
  work?: MetaWork;
  recording?: MetaRecording;
  series?: MetaSeries[];
  web_url?: string;
}

/** catalog.MatchMode*: a run over unmatched books, or a second look at community ASINs. */
export type MatchRunMode = 'match' | 'repick';

/** catalog.Match* statuses. matching and applying are working. */
export type MatchRunStatus =
  'matching' | 'ready' | 'applying' | 'applied' | 'cancelled' | 'failed' | 'interrupted';

/** catalog.Outcome*: confident, worth a look, no candidate, or the service failed. */
export type MatchOutcome = 'auto' | 'review' | 'none' | 'error';

/** matchrun.Scopes: how much applying may write, narrowest first. */
export const MATCH_SCOPES = ['ids', 'fill', 'overwrite'] as const;
export type MatchScope = (typeof MATCH_SCOPES)[number];

/** catalog.MatchRun (GET /admin/match-runs, /admin/match-runs/{id}). */
export interface MatchRun {
  id: number;
  /** null = every library. */
  library_id: number | null;
  library_name?: string;
  mode: MatchRunMode;
  region: string;
  status: MatchRunStatus;
  started_by: number | null;
  started_by_name?: string;
  started_at: string;
  finished_at: string | null;
  /** Books to match, and matched so far. */
  total: number;
  done: number;
  /** The last apply's scope ("" before one). */
  scope: MatchScope | '';
  apply_total: number;
  apply_done: number;
  applied_at: string | null;
  /** Why a failed run stopped: metadata_off | metadata_unavailable | internal. */
  error?: string;
  counts: {
    auto: number;
    /** Confident items not applied yet. */
    pending: number;
    review: number;
    none: number;
    error: number;
    applied: number;
    skipped: number;
    failed: number;
  };
}

/** catalog.ClearedMatches (DELETE /admin/community-matches): what a clear removed. */
export interface ClearedMatches {
  /** Books that had a community value or cover. */
  books: number;
  covers: number;
  /** Match runs, with their reviews. */
  runs: number;
}

/** catalog.MatchProposal: what the best community candidate offers a book. */
export interface MatchProposal {
  work_id?: string;
  recording_id?: string;
  title?: string;
  authors?: string;
  narrators?: string;
  runtime_min?: number;
  web_url?: string;
  cover_url?: string;
  /** The proposed ASIN's marketplace. */
  asin_region?: string;
  /** Each field the community has a value for, in stored form. */
  values: Partial<Record<OverrideField, string>>;
}

/** matchrun.ItemView: one book a run matched, as the review shows it. */
export interface MatchRunItem {
  id: number;
  run_id: number;
  library_id: number;
  path: string;
  outcome: MatchOutcome;
  score: number;
  /** The next candidate's score (0 = none). */
  runner_up: number;
  proposal: MatchProposal;
  /** What applying did: '' (not yet) | applied | skipped | failed. */
  applied: '' | 'applied' | 'skipped' | 'failed';
  /** Why: book_gone | nothing_to_change | edit_failed | cover_failed | metadata_unavailable. */
  detail?: string;
  /** The book as it is now. */
  book: { title: string; author: string };
  /** Nothing is indexed at the path any more. */
  gone?: boolean;
  /** What each scope would write: fields (in display order) and the cover. */
  changes: Partial<Record<MatchScope, { fields: OverrideField[]; cover: boolean }>>;
}

/** GET /admin/match-runs/{id}/items: a page, and the id the next reads after (0 = the last). */
export interface MatchRunItemPage {
  items: MatchRunItem[];
  next_after: number;
}

// Listening import (Settings > Import): another server's listening history copied
// into a person's own. v1 reads Audiobookshelf (internal/importer); admin only.

/** Where an import is: fetched in the background, reviewed, then applied (or undone). */
export type ImportStatus = 'fetching' | 'review' | 'applying' | 'applied' | 'failed' | 'undone';

/** What an import adds, counted when it was planned (recounted when the cutoff changes). */
export interface ImportSummary {
  /** Audiobookshelf books this person has any history for. */
  items: number;
  /** Books matched, by how: the same file path, then ASIN, ISBN, title and author. */
  matched: { path: number; asin: number; isbn: number; title: number };
  /** Not matched, matched twice (contested), or matched to a book the person can't see. */
  unmatched: number;
  /** Sessions it imports (matched books, before the cutoff). */
  sessions: number;
  skipped_after_cutoff: number;
  /** Seconds of listening the sessions add. */
  listened: number;
  /** Seconds added as estimates: progress beyond what the sessions cover. */
  estimated: number;
  /** Progress rows created or moved forward. */
  progress: number;
  /** Books marked finished with Audiobookshelf's date. */
  finished: number;
  bookmarks: number;
  /** The earliest and latest imported session start. */
  first_listen: string | null;
  last_listen: string | null;
}

/** One import: one Audiobookshelf user's history into one AudioSilo user. */
export interface Import {
  id: number;
  user_id: number;
  username: string;
  source: 'abs';
  /** The Audiobookshelf address as entered (never the token). */
  source_url: string;
  /** The Audiobookshelf username the history came from. */
  source_user: string;
  status: ImportStatus;
  /** Sessions starting at or after this are skipped; null = none are. */
  cutoff: string | null;
  /**
   * The server's offset from UTC at the cutoff, in minutes (null without one):
   * read the cutoff in server time, where a chosen day starts at midnight.
   */
  cutoff_utc_offset: number | null;
  created_at: string;
  applied_at: string | null;
  /** "" unless failed: a safe English sentence. */
  error: string;
  /** "" unless failed: abs_unreachable | abs_unauthorized | not_abs | interrupted | fetch_failed. */
  error_code: string;
  /** null while fetching, or when it failed before planning. */
  summary: ImportSummary | null;
}

/** An Audiobookshelf book with history that matched nothing here. */
export interface UnmatchedItem {
  title: string;
  author: string;
  /** Seconds of Audiobookshelf listening. */
  listened: number;
  sessions: number;
  reason: 'no_match' | 'contested' | 'no_access' | 'split_discs';
}

/** GET /admin/imports/{id}: the import and its unmatched books (most listened first, at most 500). */
export interface ImportDetail extends Import {
  unmatched_items: UnmatchedItem[];
}

/** An Audiobookshelf account, with the AudioSilo user of the same name (if any). */
export interface AbsUser {
  abs_id: string;
  username: string;
  /** root | admin | user | guest */
  type: string;
  suggested_user_id: number | null;
}

/**
 * One account's history going to one person (POST /admin/imports/abs).
 * abs_username names the import while it is fetched (or if it fails).
 */
export interface ImportMapping {
  abs_user_id: string;
  abs_username?: string;
  user_id: number;
}

/**
 * Which sessions an import keeps: "auto" (before each person's first AudioSilo
 * listening), null (all), or before a day (YYYY-MM-DD, the start of that day in
 * the server's time) or a moment (RFC 3339).
 */
export type ImportCutoff = 'auto' | null | string;
