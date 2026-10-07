import { browserId } from '@/lib/browser-id';
import { clearToken, getToken } from './token';
import type {
  Activity,
  AdminBookDetail,
  AdminBookPage,
  AdminBookSort,
  AuditFilter,
  AuditPage,
  BackupsEnvelope,
  NotifyTarget,
  NotifyTargetInput,
  NotifyTargetsEnvelope,
  NotifyTestResult,
  ServerEventKind,
  ServerEventPage,
  AuthorsResponse,
  BookEditRequest,
  BookFacets,
  BookMeta,
  BookRef,
  BookWorks,
  CoverThumb,
  Device,
  DuplicateGroup,
  IssueKind,
  IssuesSummary,
  Job,
  JobsState,
  LibraryRequest,
  ListeningDays,
  ListeningSession,
  MatchCandidate,
  ProgressEdit,
  ScanRun,
  ScanRunPage,
  SessionFilter,
  SessionPage,
  NarratorsResponse,
  PeopleResponse,
  PersonField,
  SeriesCount,
  AdminLibrary,
  AdminSettings,
  AdminShare,
  LogPage,
  SettingsPatch,
  SystemStatus,
  UpdateStatus,
  AdminStats,
  BookPage,
  DirListing,
  ErrorEnvelope,
  FolderMode,
  FsListing,
  Invite,
  InviteCreated,
  Library,
  LoginResponse,
  PathRule,
  ServerInfo,
  Share,
  User,
  UserDetail,
  UserProgress,
} from './types';

const API = '/api/v1';

/**
 * A failed API call: the HTTP status, the server's {"error"} message, and its
 * machine-readable `code` when the failure is one a person can fix.
 */
export class ApiError extends Error {
  readonly status: number;
  readonly code?: string;
  /** The field a refused book edit names (code "invalid_override"). */
  readonly field?: string;
  /** Which rule a refused field broke (e.g. invalid_target's notify.Reason*), and a length's limit. */
  readonly reason?: string;
  readonly max?: number;
  constructor(
    status: number,
    message: string,
    code?: string,
    field?: string,
    detail: { reason?: string; max?: number } = {},
  ) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
    this.field = field;
    this.reason = detail.reason;
    this.max = detail.max;
  }
}

let onUnauthorized: () => void = () => {};
let onForbidden: () => void = () => {};

/**
 * Registers what a 401 means for the app (drop the session, show sign-in). The
 * client clears the stored token itself before calling it.
 */
export function setUnauthorizedHandler(fn: () => void) {
  onUnauthorized = fn;
}

/**
 * Registers what a 403 from an admin endpoint means: the session is valid but
 * may no longer be an admin's (another admin demoted this account), so the app
 * re-checks who is signed in.
 */
export function setForbiddenHandler(fn: () => void) {
  onForbidden = fn;
}

const LOGIN_PATH = '/auth/login';

/** How the console names itself to the server (X-AudioSilo-Client), so the People and Activity
 * screens can tell a console session from a player. It ships inside the server, so it sends no
 * version of its own. Always same-origin, so the header never costs a CORS preflight. */
export const CLIENT_APP = 'AudioSilo Admin';
export const CLIENT_IDENTITY = `${CLIENT_APP} (web)`;

/**
 * Every API call goes through here: the bearer header, what a 401/403 means for
 * the session, and a failure raised as ApiError from the {"error"} envelope.
 * `explicitToken` authenticates with a token that is not the stored session
 * (signing a non-admin straight back out) and leaves the stored session alone.
 * `raw` sends a file as the body as is (a cover upload) instead of JSON.
 */
async function send(
  path: string,
  init: {
    method?: string;
    body?: unknown;
    raw?: Blob;
    explicitToken?: string;
  } = {},
): Promise<Response> {
  const headers: Record<string, string> = { 'X-AudioSilo-Client': CLIENT_IDENTITY };
  const token = init.explicitToken ?? getToken();
  if (token) headers.Authorization = `Bearer ${token}`;
  if (init.raw) headers['Content-Type'] = init.raw.type || 'application/octet-stream';
  else if (init.body !== undefined) headers['Content-Type'] = 'application/json';
  const res = await fetch(API + path, {
    method: init.method ?? 'GET',
    headers,
    body: init.raw ?? (init.body === undefined ? undefined : JSON.stringify(init.body)),
  });
  if (init.explicitToken === undefined) checkSession(res.status, path);
  if (!res.ok) throw await apiError(res);
  return res;
}

async function apiError(res: Response): Promise<ApiError> {
  let env: Partial<ErrorEnvelope> | undefined;
  try {
    env = JSON.parse(await res.text()) as Partial<ErrorEnvelope>;
  } catch {
    // a non-JSON body (a proxy error page): fall back to the status text below
  }
  const msg =
    typeof env?.error === 'string' && env.error
      ? env.error
      : res.statusText || `HTTP ${res.status}`;
  return new ApiError(
    res.status,
    msg,
    typeof env?.code === 'string' ? env.code : undefined,
    typeof env?.field === 'string' ? env.field : undefined,
    {
      reason: typeof env?.reason === 'string' ? env.reason : undefined,
      max: typeof env?.max === 'number' ? env.max : undefined,
    },
  );
}

/** A JSON API call (an empty body decodes to undefined). */
async function request<T>(
  method: string,
  path: string,
  body?: unknown,
  explicitToken?: string,
): Promise<T> {
  const text = await (await send(path, { method, body, explicitToken })).text();
  try {
    return (text ? JSON.parse(text) : undefined) as T;
  } catch {
    return undefined as T; // a 2xx with a non-JSON body (a proxy page): nothing to decode
  }
}

/** What a 401 or 403 on the stored session means for the app (see the setters). */
function checkSession(status: number, path: string) {
  // Any 401 but a failed sign-in ends the session, including one sent with no
  // token at all: another tab signed out and cleared it.
  if (status === 401 && path !== LOGIN_PATH) {
    clearToken();
    onUnauthorized();
  }
  if (status === 403 && path.startsWith('/admin/')) onForbidden();
}

/** The path query every content endpoint takes (path is the identity, never an id). */
const pathQuery = (path: string) => `?${new URLSearchParams({ path }).toString()}`;

/**
 * The filters GET /admin/books and /admin/books/facets share
 * (handlers_catalog.go bookFilterFromQuery). Unset = no filter.
 */
export interface BookFilter {
  /** Full text over title, author, series and narrator. */
  q?: string;
  library_id?: number;
  /** Exact effective values (an author tile, a series card). */
  author?: string;
  series?: string;
  narrator?: string;
  format?: string[];
  codec?: string[];
  direct_playable?: boolean;
  has_cover?: boolean;
  has_chapters?: boolean;
  matched?: boolean;
  edited?: boolean;
  /** Seconds. */
  min_duration?: number;
  max_duration?: number;
  /** YYYY-MM-DD or RFC 3339; after is inclusive, before exclusive. */
  added_after?: string;
  added_before?: string;
  /** One Health issue's books (not duplicate), without the ignored ones, or only those. */
  issue?: Exclude<IssueKind, 'duplicate'>;
  issue_ignored?: boolean;
}

/** One page request of GET /admin/books. */
export interface BookListParams extends BookFilter {
  sort?: AdminBookSort;
  order?: 'asc' | 'desc';
  cursor?: string;
  /** 1-200; the server defaults to 60. */
  limit?: number;
}

/**
 * Parameters as a query string ("" when empty), unset values left out and arrays
 * repeated (?format=mp3&format=m4b), the way the server parses them.
 */
export function bookQuery(params: object): string {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(params) as [string, unknown][]) {
    if (v === undefined || v === '' || v === null) continue;
    if (Array.isArray(v)) for (const item of v) q.append(k, String(item));
    else q.set(k, String(v));
  }
  const s = q.toString();
  return s ? `?${s}` : '';
}

/** GET /admin/logs filters (handlers_system.go handleLogs). */
export interface LogQuery {
  /** At least this level. */
  level?: 'info' | 'warn' | 'error';
  /** Lines containing this (any case). */
  q?: string;
  /** Only lines after this seq (the live tail's cursor). */
  after?: number;
  /** At most this many, the newest (<= 1000). */
  limit?: number;
}

/** What GET .../book/match searches by ({} = the book's own facts). */
export type MatchBy = { q?: string; asin?: string; isbn?: string };

/** The cover thumbnail sizes POST /admin/covers renders (handlers_covers.go). */
export type ThumbSize = 160 | 320 | 640;

/** The server's cap on one POST /admin/meta/covers (handlers_community_covers.go). */
export const COMMUNITY_COVERS_LIMIT = 12;
/** The server's cap on one bulk edit (handlers_catalog.go), applied all or nothing. */
export const BULK_LIMIT = 1000;
/** The server's cap on the books one POST /admin/books/works resolves (handlers_catalog.go). */
export const WORKS_LIMIT = 100;
/** The server's cap on the rules one POST /admin/shares/{id}/paths adds (handlers_shares.go). */
export const SHARE_RULES_LIMIT = 1000;

export const api = {
  serverInfo: () => request<ServerInfo>('GET', '/server'),
  login: (username: string, password: string) =>
    request<LoginResponse>('POST', LOGIN_PATH, {
      username,
      password,
      device_name: 'admin-web',
      device_id: browserId(),
    }),
  /** Revokes the stored session, or `token` when given (never touching storage). */
  logout: (token?: string) => request<void>('POST', '/auth/logout', undefined, token),
  me: () => request<User>('GET', '/me'),
  stats: () => request<AdminStats>('GET', '/admin/stats'),
  settings: () => request<AdminSettings>('GET', '/admin/settings'),
  /** Queues a scan (coalesced with one already waiting); answers with the job. */
  scanLibrary: (id: number) =>
    request<{ status: string; job: Job }>('POST', `/admin/libraries/${id}/scan`),
  /** Queues a scan of every library; the queue runs them one at a time. */
  scanAll: () => request<{ jobs: Job[] }>('POST', '/admin/scan'),
  updateSettings: (patch: SettingsPatch) =>
    request<AdminSettings>('PATCH', '/admin/settings', patch),
  system: () => request<SystemStatus>('GET', '/admin/system'),
  updateStatus: () => request<UpdateStatus>('GET', '/admin/update'),
  checkForUpdate: () => request<UpdateStatus>('POST', '/admin/update/check'),
  /** The newest log lines; `after` (a previous page's last_seq) asks only for newer ones. */
  logs: (q: LogQuery = {}) => request<LogPage>('GET', `/admin/logs${bookQuery(q)}`),

  // Backups: the database in the backups folder; a restore applies at the next start.
  backups: () => request<BackupsEnvelope>('GET', '/admin/backups'),
  /** Starts a backup (202; 409 backup_running while one is made); answers with the list. */
  createBackup: () => request<BackupsEnvelope>('POST', '/admin/backups'),
  deleteBackup: (name: string) =>
    request<void>('DELETE', `/admin/backups/${encodeURIComponent(name)}`),
  /** Marks a backup to be restored at the next start (400 invalid_backup / backup_too_new). */
  restoreBackup: (name: string) =>
    request<BackupsEnvelope>('POST', `/admin/backups/${encodeURIComponent(name)}/restore`),
  cancelRestore: () => request<void>('DELETE', '/admin/restore'),

  // Notifications and the event feed.
  notifyTargets: () => request<NotifyTargetsEnvelope>('GET', '/admin/notifications'),
  createNotifyTarget: (t: NotifyTargetInput) =>
    request<NotifyTarget>('POST', '/admin/notifications', t),
  /** Only the fields sent change; an absent url or secret is kept, secret "" clears it. */
  updateNotifyTarget: (id: number, t: NotifyTargetInput) =>
    request<NotifyTarget>('PATCH', `/admin/notifications/${id}`, t),
  deleteNotifyTarget: (id: number) => request<void>('DELETE', `/admin/notifications/${id}`),
  testNotifyTarget: (id: number) =>
    request<NotifyTestResult>('POST', `/admin/notifications/${id}/test`),
  /** The bell's feed, newest first; `before` is the previous page's next_before. */
  serverEvents: (opts: { before?: number; limit?: number; kind?: ServerEventKind } = {}) =>
    request<ServerEventPage>('GET', `/admin/events${bookQuery(opts)}`),

  /** The audit log, newest first; `before` is the previous page's next_before. */
  audit: (opts: AuditFilter & { before?: number; limit?: number } = {}) =>
    request<AuditPage>('GET', `/admin/audit${bookQuery(opts)}`),

  libraries: () => request<{ libraries: AdminLibrary[] }>('GET', '/admin/libraries'),
  createLibrary: (lib: LibraryRequest & { name: string; root: string }) =>
    request<Library>('POST', '/admin/libraries', lib),
  /** Fields left out are kept; a new root or new ignore rules queue a rescan (`job`). */
  updateLibrary: (id: number, lib: LibraryRequest) =>
    request<Library & { job?: Job }>('PATCH', `/admin/libraries/${id}`, lib),
  deleteLibrary: (id: number) => request<void>('DELETE', `/admin/libraries/${id}`),
  reorderLibraries: (ids: number[]) =>
    request<{ libraries: AdminLibrary[] }>('PUT', '/admin/libraries/order', { ids }),
  setFolderOverride: (id: number, path: string, mode: FolderMode | null) =>
    mode
      ? request<unknown>('PUT', `/admin/libraries/${id}/folder-override${pathQuery(path)}`, {
          mode,
        })
      : request<unknown>('DELETE', `/admin/libraries/${id}/folder-override${pathQuery(path)}`),
  /** A folder of a library, as the player browses it (books annotated, overrides shown). */
  browse: (libraryId: number, path: string) =>
    request<FsListing>(
      'GET',
      `/libraries/${libraryId}/fs?${new URLSearchParams({ path, limit: '500' }).toString()}`,
    ),
  /** The newest books of a library (covers for its card). */
  recentBooks: (libraryId: number, limit: number) =>
    request<BookPage>('GET', `/libraries/${libraryId}/books?sort=recent&limit=${limit}`),
  /** The server's folders, for choosing a library root (absolute paths; "" = the filesystem root). */
  dirs: (path: string) => request<DirListing>('GET', `/admin/fs/dirs${pathQuery(path)}`),

  users: () => request<{ users: User[] }>('GET', '/admin/users'),
  user: (id: number) => request<UserDetail>('GET', `/admin/users/${id}`),
  createUser: (u: { username: string; password: string; role: User['role'] }) =>
    request<User>('POST', '/admin/users', u),
  updateUser: (id: number, patch: { role?: User['role']; password?: string; disabled?: boolean }) =>
    request<User>('PATCH', `/admin/users/${id}`, patch),
  deleteUser: (id: number) => request<void>('DELETE', `/admin/users/${id}`),
  clearRecovery: (id: number) => request<void>('DELETE', `/admin/users/${id}/recovery`),
  invites: () => request<{ invites: Invite[] }>('GET', '/admin/invites'),
  /** Mints an invite; `max_uses` 0 = unlimited, `ttl_days` 0 = never expires. */
  createInvite: (userId: number, opts: { max_uses: number; ttl_days: number }) =>
    request<InviteCreated>('POST', `/admin/users/${userId}/authcode`, {
      label: 'invite',
      ...opts,
    }),
  rotateInvite: (id: number) => request<InviteCreated>('POST', `/admin/authcodes/${id}/rotate`),
  revokeInvite: (id: number) => request<void>('DELETE', `/admin/authcodes/${id}`),

  shares: () => request<{ shares: AdminShare[] }>('GET', '/admin/shares'),
  createShare: (name: string) => request<Share>('POST', '/admin/shares', { name }),
  renameShare: (id: number, name: string) =>
    request<Share>('PATCH', `/admin/shares/${id}`, { name }),
  deleteShare: (id: number) => request<void>('DELETE', `/admin/shares/${id}`),
  addSharePath: (id: number, rule: PathRule) =>
    request<void>('POST', `/admin/shares/${id}/paths`, rule),
  /** Adds many rules in one transaction, all or nothing (1 to SHARE_RULES_LIMIT). */
  addSharePaths: (id: number, rules: PathRule[]) =>
    request<void>('POST', `/admin/shares/${id}/paths`, { rules }),
  removeSharePath: (id: number, rule: PathRule) =>
    request<void>('DELETE', `/admin/shares/${id}/paths`, rule),
  grantShare: (userId: number, shareId: number) =>
    request<void>('POST', '/admin/share-access', { user_id: userId, share_id: shareId }),
  revokeShare: (userId: number, shareId: number) =>
    request<void>('DELETE', '/admin/share-access', { user_id: userId, share_id: shareId }),
  grantLibrary: (userId: number, libraryId: number) =>
    request<void>('POST', '/admin/library-access', { user_id: userId, library_id: libraryId }),

  // The admin catalog (Library and Book screens).
  adminBooks: (params: BookListParams) =>
    request<AdminBookPage>('GET', `/admin/books${bookQuery(params)}`),
  bookFacets: (filter: BookFilter) =>
    request<BookFacets>('GET', `/admin/books/facets${bookQuery(filter)}`),
  /** One field edit over many books, all or nothing (at most BULK_LIMIT). */
  bulkEdit: (books: BookRef[], edit: Omit<BookEditRequest, 'chapters'>) =>
    request<{ updated: number }>('POST', '/admin/books/bulk', { books, ...edit }),
  /** GET /admin/authors or /admin/narrators, as one shape. */
  people: async (field: PersonField, libraryId?: number): Promise<PeopleResponse> => {
    const query = bookQuery({ library_id: libraryId });
    const r =
      field === 'author'
        ? await request<AuthorsResponse>('GET', `/admin/authors${query}`)
        : await request<NarratorsResponse>('GET', `/admin/narrators${query}`);
    return {
      people: ('authors' in r ? r.authors : r.narrators) ?? [],
      merge_suggestions: r.merge_suggestions ?? [],
      unknown: r.unknown ?? 0,
    };
  },
  series: (libraryId?: number) =>
    request<{ series: SeriesCount[] }>(
      'GET',
      `/admin/series${bookQuery({ library_id: libraryId })}`,
    ),
  adminBook: (libraryId: number, path: string) =>
    request<AdminBookDetail>('GET', `/admin/libraries/${libraryId}/book${pathQuery(path)}`),
  /** Sets or reverts overrides; answers with the updated book page. */
  editBook: (libraryId: number, path: string, edit: BookEditRequest) =>
    request<AdminBookDetail>('PATCH', `/admin/libraries/${libraryId}/book${pathQuery(path)}`, edit),
  /** Community works the book might be: by its own facts, or by `q` / an ASIN / an ISBN. */
  matchBook: (libraryId: number, path: string, by: MatchBy = {}) =>
    request<{ candidates: MatchCandidate[] }>(
      'GET',
      `/admin/libraries/${libraryId}/book/match${bookQuery({ path, ...by })}`,
    ),
  /** The book's community metadata (series rails for the Series gaps). */
  bookMeta: (libraryId: number, path: string) =>
    request<BookMeta>('GET', `/libraries/${libraryId}/meta${pathQuery(path)}`),
  /** Which community work each book is, in request order (at most WORKS_LIMIT). */
  bookWorks: (books: BookRef[]) => request<BookWorks>('POST', '/admin/books/works', { books }),
  /** Cover thumbnails as data: URLs, in request order (at most 60). */
  coverThumbs: (books: BookRef[], size: ThumbSize = 320) =>
    request<{ covers: CoverThumb[] }>('POST', '/admin/covers', { books, size }),
  /** Uploads a custom cover (JPEG, PNG or WebP, at most 5 MiB); the book folder is untouched. */
  setCover: async (libraryId: number, path: string, image: Blob) => {
    await send(`/admin/libraries/${libraryId}/cover${pathQuery(path)}`, {
      method: 'PUT',
      raw: image,
    });
  },
  /** Thumbnails of community cover images (match candidates' cover_url), as data: URLs or "", in order (at most COMMUNITY_COVERS_LIMIT). */
  communityCovers: (urls: string[], size: ThumbSize = 160) =>
    request<{ covers: string[] }>('POST', '/admin/meta/covers', { urls, size }),
  /** Fetches a community cover on the server and keeps it as the book's custom cover. */
  setCommunityCover: (libraryId: number, path: string, url: string) =>
    request<void>('PUT', `/admin/libraries/${libraryId}/cover/community${pathQuery(path)}`, {
      url,
    }),
  deleteCover: (libraryId: number, path: string) =>
    request<void>('DELETE', `/admin/libraries/${libraryId}/cover${pathQuery(path)}`),
  /** Reads one book's files again now; answers with its page (404 not_indexable if it's gone). */
  rescanBook: (libraryId: number, path: string) =>
    request<AdminBookDetail>('POST', `/admin/libraries/${libraryId}/book/rescan${pathQuery(path)}`),

  // Health and Jobs.
  issues: () => request<IssuesSummary>('GET', '/admin/issues'),
  duplicates: (opts: { library_id?: number; ignored?: boolean } = {}) =>
    request<{ groups: DuplicateGroup[] }>('GET', `/admin/issues/duplicates${bookQuery(opts)}`),
  /** Stops showing these books under a category (at most BULK_LIMIT). */
  ignoreIssue: (kind: IssueKind, books: BookRef[]) =>
    request<void>('POST', '/admin/issues/ignore', { kind, books }),
  unignoreIssue: (kind: IssueKind, books: BookRef[]) =>
    request<void>('DELETE', '/admin/issues/ignore', { kind, books }),
  jobs: () => request<JobsState>('GET', '/admin/jobs'),
  /** Drops a queued scan or stops the running one (it stops before removing anything). */
  cancelJob: (id: number) => request<void>('DELETE', `/admin/jobs/${id}`),
  scanRuns: (opts: { library_id?: number; before?: number; limit?: number } = {}) =>
    request<ScanRunPage>('GET', `/admin/scan-runs${bookQuery(opts)}`),
  scanRun: (id: number) => request<ScanRun>('GET', `/admin/scan-runs/${id}`),

  // Activity: sessions, devices, progress edits and listening stats.
  /** The Activity page for a period ("7d", "30d", "90d", "1y" or a year), in server time. */
  activity: async (range: string): Promise<Activity> => {
    const r = await request<{ activity?: Activity }>('GET', `/admin/stats${bookQuery({ range })}`);
    // A server from before the Activity page ignores ?range=: say so, don't draw an empty page.
    if (!r?.activity) throw new ApiError(500, 'the server sent no activity');
    return r.activity;
  },
  /** Listening per day over a period, of everyone or one person (no other stats). */
  listeningDays: (range: string, userId?: number) =>
    request<ListeningDays>('GET', `/admin/listening${bookQuery({ range, user_id: userId })}`),
  /** Who is listening now: one session per device, with chapter and address. */
  liveSessions: () => request<{ sessions: ListeningSession[] }>('GET', '/admin/sessions/live'),
  /** Sessions newest first; `before` is the previous page's next_before (1-200 a page). */
  sessions: (opts: SessionFilter & { before?: number; limit?: number } = {}) =>
    request<SessionPage>('GET', `/admin/sessions${bookQuery(opts)}`),
  /** Signed-in devices and API keys, of one person or everyone. */
  devices: (userId?: number) =>
    request<{ devices: Device[] }>('GET', `/admin/devices${bookQuery({ user_id: userId })}`),
  /** Signs one device out (409 current_device for the console's own). */
  revokeDevice: (id: number) => request<void>('DELETE', `/admin/devices/${id}`),
  /** Every book a person has progress on, most recently saved first. */
  userProgress: (userId: number) =>
    request<{ progress: UserProgress[] }>('GET', `/admin/users/${userId}/progress`),
  /** Changes a person's progress on a book (409 no_access to start one they can't see). */
  editProgress: (libraryId: number, path: string, userId: number, edit: ProgressEdit) =>
    request<{ progress: UserProgress }>(
      'PATCH',
      `/admin/libraries/${libraryId}/progress${bookQuery({ path, user_id: userId })}`,
      edit,
    ),
};

/**
 * Downloads a library's book list (GET /admin/libraries/{id}/export) as the file
 * the server names. Returns the file name.
 */
export function downloadLibraryExport(id: number): Promise<string> {
  return download(`/admin/libraries/${id}/export`, `audiosilo-library-${id}.json`);
}

/** Downloads a backup (GET /admin/backups/{name}). Returns the file name. */
export function downloadBackup(name: string): Promise<string> {
  return download(`/admin/backups/${encodeURIComponent(name)}`, name);
}

/**
 * Saves what an API path answers as the file the server names (fallback: `fallback`).
 * It can't be a plain link: the request needs the Authorization header, so the body
 * is fetched and handed to the browser as an object URL (a download, not a resource
 * load, so the CSP doesn't apply).
 */
async function download(path: string, fallback: string): Promise<string> {
  const res = await send(path);
  const name =
    /filename="([^"]*)"/i.exec(res.headers.get('Content-Disposition') ?? '')?.[1]?.trim() ||
    fallback;
  const url = URL.createObjectURL(await res.blob());
  const a = document.createElement('a');
  a.href = url;
  a.download = name;
  document.body.append(a);
  a.click();
  a.remove();
  // Revoking at once can cancel the save in some browsers.
  setTimeout(() => URL.revokeObjectURL(url), 10_000);
  return name;
}
