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
}

/** library.ScanProgress (internal/library/scanner.go), GET /admin/libraries/{id}/scan. */
export interface ScanProgress {
  running: boolean;
  total: number;
  done: number;
  indexed: number;
  /** The last finished scan stopped at the unavailable-root guard (nothing pruned). */
  unavailable?: boolean;
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
}
