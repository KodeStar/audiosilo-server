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

/** The {"error": "..."} envelope every handler uses for failures (respond.go). */
export interface ErrorEnvelope {
  error: string;
}
