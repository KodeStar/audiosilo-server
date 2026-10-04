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
  state: SessionState;
  /** Live sessions only: the chapter at the position and the device's newest address. */
  chapter?: string;
  ip?: string;
}

/** What a session list is narrowed to (GET /admin/sessions). */
export interface SessionFilter {
  user_id?: number;
  library_id?: number;
  path?: string;
}

/** GET /admin/sessions: newest first; `next_before` asks for the next page (null at the end). */
export interface SessionPage {
  sessions: ListeningSession[];
  next_before: number | null;
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

/** GET/PATCH /api/v1/admin/settings (handlers_settings.go settingsEnvelope). */
export interface AdminSettings {
  metadata: {
    enabled: boolean;
    base_url: string;
    available: boolean;
  };
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
  /** When the next scheduled scan is due (RFC 3339); absent without a schedule. */
  next_scan_at?: string;
}

/** The body of POST /admin/libraries and PATCH /admin/libraries/{id} (handlers_admin.go libraryRequest). */
export interface LibraryRequest {
  name?: string;
  root?: string;
  scan_schedule?: string;
  ignore_patterns?: string[];
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
  'duplicate',
  'no_cover',
  'unmatched',
  'no_chapters',
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
}

// ---- Admin catalog (Phase 2a API, consumed by the Library and Book screens) ----

/** One row of GET /admin/books (catalog.AdminBook, internal/catalog/adminbooks.go). */
export interface AdminBook {
  library_id: number;
  library_name: string;
  path: string;
  is_folder: boolean;
  title: string;
  author: string;
  narrator: string;
  series: string;
  series_index: number;
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
  'series',
  'narrator',
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

/** catalog.PersonCount: one author or narrator (a whole field value). */
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

/** The two people aggregates (a whole field value is one person). */
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
}

/** The overridable book fields (catalog.OverrideFields), in display order. */
export const OVERRIDE_FIELDS = [
  'title',
  'author',
  'narrator',
  'series',
  'series_index',
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
}

/** The body of PATCH /admin/libraries/{id}/book (handlers_catalog.go editRequest). */
export interface BookEditRequest {
  set?: Partial<Record<OverrideField, string>>;
  revert?: OverrideField[];
  /** "community" when the values were accepted from a match; default "edited". */
  source?: 'edited' | 'community';
  chapters?: { set?: Record<number, string>; revert?: number[] };
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
  asins: string[];
  isbns: string[];
  cover_url?: string;
}

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
  /** 0-100: how well the work fits the book (100 = identifier hit). */
  score: number;
}

/** One entry of POST /admin/covers (handlers_covers.go coverThumb), in request order. */
export interface CoverThumb {
  library_id: number;
  path: string;
  /** A data: URL of a JPEG thumbnail, or "" when the book has no art. */
  data: string;
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
