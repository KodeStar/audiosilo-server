import { browserId } from '@/lib/browser-id';
import { clearToken, getToken } from './token';
import type {
  AbsUser,
  Activity,
  AdminBookDetail,
  AdminBookPage,
  AdminBookSort,
  AudibleAccount,
  AudibleAccountPatch,
  AudibleAccounts,
  AudibleTitle,
  AudibleTitlePage,
  AudibleTitlePatch,
  AudibleTitleQuery,
  AuditFilter,
  AuditPage,
  BackupsEnvelope,
  BridgeEstimate,
  BridgeJob,
  ChapterEditRequest,
  BridgeJobPage,
  BridgeJobRequest,
  BridgePairCode,
  BridgeStatus,
  LibrivoxHit,
  LibrivoxWork,
  NotifyTarget,
  NotifyTargetInput,
  NotifyTargetsEnvelope,
  NotifyTestResult,
  ServerEventKind,
  ServerEventPage,
  AuthorsResponse,
  BookEditRequest,
  BookFacets,
  ListeningActivity,
  BookMeta,
  BookRef,
  BookWorks,
  ClearedMatches,
  CoverThumb,
  Device,
  EndedKeys,
  DuplicatePage,
  IssueKind,
  SeriesSpellingGroup,
  IssuesSummary,
  Job,
  JobsState,
  LibraryPreview,
  LibraryPreviewRequest,
  LibraryRequest,
  ListeningDays,
  ListeningSession,
  MatchCandidate,
  MatchOutcome,
  MatchRun,
  MatchRunList,
  MatchRunItemPage,
  MatchRunMode,
  MatchApplyRequest,
  BookMatchProposal,
  ProgressEdit,
  ReadalongEstimate,
  ScanRun,
  ScanRunPage,
  SessionCursor,
  SessionFilter,
  SessionPage,
  NarratorsResponse,
  PeopleResponse,
  MergedBook,
  MergeField,
  MergeUndone,
  PersonField,
  SeriesCount,
  AdminLibrary,
  AdminModules,
  AdminSettings,
  AdminUser,
  AdminShare,
  LogPage,
  SettingsPatch,
  SystemStatus,
  SupportAction,
  SupportStatus,
  UpdateStatus,
  AdminStats,
  BookPage,
  DirListing,
  ErrorEnvelope,
  FolderMode,
  FsListing,
  Import,
  ImportCutoff,
  ImportDetail,
  ImportMapping,
  Invite,
  InviteCreated,
  Library,
  LoginResponse,
  MetaMirrorStatus,
  OIDCPendingList,
  OIDCStatus,
  PairToken,
  PathRule,
  ServerInfo,
  SetupEnvelope,
  SetupPatch,
  SetupState,
  Share,
  User,
  UserPatch,
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
  /** Which rule a refused chapter edit broke (code "chapters_invalid": empty, order, ...). */
  readonly detail?: string;
  constructor(
    status: number,
    message: string,
    code?: string,
    field?: string,
    detail: { reason?: string; max?: number; detail?: string } = {},
  ) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
    this.field = field;
    this.reason = detail.reason;
    this.max = detail.max;
    this.detail = detail.detail;
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
/** A single sign-on becomes a session here (handlers_oidc.go handleOIDCFinish). */
const OIDC_FINISH_PATH = '/auth/oidc/finish';
/** The 401 of the account's own password change that refuses the current password typed. */
export const WRONG_CURRENT_PASSWORD = 'current_password_incorrect';

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
  if (res.ok) return res;
  const err = await apiError(res);
  if (init.explicitToken === undefined) checkSession(err, path);
  throw err;
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
      detail: typeof env?.detail === 'string' ? env.detail : undefined,
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
function checkSession(err: ApiError, path: string) {
  // Any 401 but a failed sign-in or a wrong current password (the session is
  // fine: the password typed wasn't) ends the session, including one sent with
  // no token at all: another tab signed out and cleared it.
  if (
    err.status === 401 &&
    path !== LOGIN_PATH &&
    path !== OIDC_FINISH_PATH &&
    err.code !== WRONG_CURRENT_PASSWORD
  ) {
    clearToken();
    onUnauthorized();
  }
  if (err.status === 403 && path.startsWith('/admin/')) onForbidden();
}

/** The path query every content endpoint takes (path is the identity, never an id). */
const pathQuery = (path: string) => `?${new URLSearchParams({ path }).toString()}`;

/**
 * An audio file's stream URL for an <audio> element: `token` is a book media token
 * (api.bookMediaToken), never the session's, which would leak into logs and history.
 * With `transcodeFrom`, the file's live MP3 transcode from that many seconds into
 * it (?transcode=1&t=), for a file no browser plays as stored.
 */
export const streamUrl = (
  libraryId: number,
  path: string,
  token: string,
  transcodeFrom?: number,
) => {
  const q = new URLSearchParams({ path, token });
  if (transcodeFrom !== undefined) {
    q.set('transcode', '1');
    q.set('t', String(transcodeFrom));
  }
  return `${API}/libraries/${libraryId}/stream?${q.toString()}`;
};

/** What the server says of a transcode its <audio> failed or stalled on. */
export interface TranscodeLook {
  /**
   * How long to wait before asking for it again, in seconds: the server's
   * Retry-After when it has no transcode slot free for it, 0 when the stream
   * would play now.
   */
  wait: number;
  /**
   * The bytes the same stream's transcode (this token, file and start) has
   * sent so far, when one is running (X-AudioSilo-Transcode-Sent): a count
   * that grows between looks is a slow transcode still on its way, which
   * asking for again would end (the new ask takes its slot over).
   */
  sent?: number;
}

/**
 * Asks the server about a transcode after its <audio> failed or stalled (an
 * <audio> can't read the answer's status): undefined for any answer but a
 * play-now or a 503 with a Retry-After (a token that ran out, a file gone, no
 * ffmpeg) or none. Asked with a HEAD, which the server answers as it would the
 * GET without starting a transcode (an ffmpeg, holding one of its few slots).
 * Never from the HTTP cache: Chromium answers a look at the URL its <audio> is
 * fetching only once that fetch lets go of the cache entry (20 s late, seen
 * live), so the look bypasses the cache with 'no-store'.
 */
export async function lookAtTranscode(
  url: string,
  signal?: AbortSignal,
): Promise<TranscodeLook | undefined> {
  try {
    const res = await fetch(url, { method: 'HEAD', cache: 'no-store', signal });
    if (res.ok) {
      const sent = res.headers.get(TRANSCODE_SENT_HEADER);
      const n = sent === null ? NaN : Number(sent);
      return Number.isFinite(n) && n >= 0 ? { wait: 0, sent: n } : { wait: 0 };
    }
    const wait = Number(res.headers.get('Retry-After'));
    if (res.status !== 503 || !Number.isFinite(wait) || wait <= 0) return undefined;
    return { wait: Math.min(wait, TRANSCODE_RETRY_MAX_S) };
  } catch {
    return undefined;
  }
}

/** The server's word on a transcode's running stream, on a HEAD of it (lookAtTranscode). */
const TRANSCODE_SENT_HEADER = 'X-AudioSilo-Transcode-Sent';

/** The longest wait a busy transcode's Retry-After is taken at its word for (seconds). */
const TRANSCODE_RETRY_MAX_S = 30;

/**
 * The filters GET /admin/books and /admin/books/facets share
 * (handlers_catalog.go bookFilterFromQuery). Unset = no filter.
 */
export interface BookFilter {
  /** Full text over title, author, series and narrator. */
  q?: string;
  library_id?: number;
  /**
   * An author tile, a series card: series is the exact effective value; author and
   * narrator also find the co-credits naming that person.
   */
  author?: string;
  series?: string;
  narrator?: string;
  /** A genre or a tag the book has, in any case. */
  genre?: string;
  tag?: string;
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

/** GET /admin/stats?range= (handleActivityStats): the period's `activity` from its envelope. */
async function activityStats<T>(query: { range: string; parts?: 'listening' }): Promise<T> {
  const r = await request<{ activity?: T }>('GET', `/admin/stats${bookQuery(query)}`);
  // A server from before the Activity page ignores ?range=: say so, don't draw an empty page.
  if (!r?.activity) throw new ApiError(500, 'the server sent no activity');
  return r.activity;
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

/** The cover thumbnail sizes POST /admin/covers renders (handlers_covers.go); 96 for table rows. */
export type ThumbSize = 96 | 160 | 320 | 640;

/** The server's cap on one POST /admin/meta/covers (handlers_community_covers.go). */
export const COMMUNITY_COVERS_LIMIT = 12;
/** The server's cap on one bulk edit (handlers_catalog.go), applied all or nothing. */
export const BULK_LIMIT = 1000;
/** The server's cap on the books one POST /admin/books/works resolves (handlers_catalog.go). */
export const WORKS_LIMIT = 100;
/** The server's cap on the rules one POST /admin/shares/{id}/paths adds (handlers_shares.go). */
export const SHARE_RULES_LIMIT = 1000;

export const api = {
  addresses: () => request<{ home?: string; away?: string }>('GET', '/addresses'),
  serverInfo: () => request<ServerInfo>('GET', '/server'),
  login: (username: string, password: string) =>
    request<LoginResponse>('POST', LOGIN_PATH, {
      username,
      password,
      device_name: 'admin-web',
      device_id: browserId(),
    }),
  /**
   * Finishes a single sign-on (OIDC) for a session (/admin/#oidc=done,
   * handlers_oidc.go): the server reads the sign-in from its flow's HttpOnly
   * cookie, once. `flow`: the flow's key the return named (sent only when it
   * did), so the sign-in finished is this tab's own. A 401 here (code
   * oidc_no_sign_in or oidc_sign_in_expired) is a sign-in this browser doesn't
   * hold, never the stored session's end.
   */
  oidcFinish: (flow?: string) =>
    request<LoginResponse>('POST', OIDC_FINISH_PATH, { device_name: 'admin-web', flow }),
  /** Revokes the stored session, or `token` when given (never touching storage). */
  logout: (token?: string) => request<void>('POST', '/auth/logout', undefined, token),
  me: () => request<User>('GET', '/me'),
  /**
   * Sets the signed-in account's own password. `current` is required while the
   * account has one (401 WRONG_CURRENT_PASSWORD when wrong); the session making
   * the change stays signed in, the others end. Answers how many of the
   * account's other sessions it ended (server#285, `{signed_out}`).
   */
  setOwnPassword: (password: string, current: string) =>
    request<{ signed_out: number }>('POST', '/auth/password', {
      password,
      current_password: current,
    }),
  /**
   * A single-use pairing token for the signed-in admin, which "Listen in
   * player" hands the web player (403 for an API key). `ttlSeconds` asks for a
   * shorter life than the server's 10 minutes (30 s at the least); an older
   * server ignores it and gives the 10 minutes.
   */
  pairSelf: (ttlSeconds?: number) =>
    request<PairToken>(
      'POST',
      '/auth/pair',
      ttlSeconds === undefined ? undefined : { ttl_seconds: ttlSeconds },
    ),
  stats: () => request<AdminStats>('GET', '/admin/stats'),
  settings: () => request<AdminSettings>('GET', '/admin/settings'),
  checkUpcoming: () => request<void>('POST', '/admin/upcoming/check'),
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
  /**
   * The local metadata copy's status alone (light: what a download's progress polls).
   * 404 metadata_off while metadata is off, 409 not_mirror_mode while the server
   * isn't keeping a copy; checkMetaMirror answers the same way.
   */
  metaMirror: () => request<MetaMirrorStatus>('GET', '/admin/meta/mirror'),
  /** Wakes the local copy's check (202, answering with its status: the next check due now). */
  checkMetaMirror: () => request<MetaMirrorStatus>('POST', '/admin/meta/mirror/check'),
  /** Whether the Overview's support card shows (the server decides when). */
  support: () => request<SupportStatus>('GET', '/admin/support'),
  /** An admin's answer to the support card, for the whole server; taken on trust. */
  answerSupport: (action: SupportAction) =>
    request<SupportStatus>('POST', '/admin/support', { action }),
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

  /** Settings > Modules: every managed module, its toggle and where it stands with the Bridge. */
  modules: () => request<AdminModules>('GET', '/admin/modules'),

  // The Bridge (Settings > Bridge): pairing, its status, and its jobs.
  bridge: () => request<BridgeStatus>('GET', '/admin/bridge'),
  /** A pairing code, shown once; it replaces one still waiting (403 for an API key). */
  pairBridge: () => request<BridgePairCode>('POST', '/admin/bridge/pair'),
  /** Revokes the Bridge's token; its jobs stay, queued ones wait for the next Bridge. */
  unpairBridge: () => request<void>('DELETE', '/admin/bridge'),
  /** Jobs newest first; `before` is the previous page's next_before. */
  bridgeJobs: (
    opts: {
      state?: string;
      /** One kind's jobs ("podcast.poll"). */
      kind?: string;
      before?: number;
      limit?: number;
      /** One book's jobs (with `path`), or one library's. */
      library_id?: number;
      path?: string;
    } = {},
  ) => request<BridgeJobPage>('GET', `/admin/bridge/jobs${bookQuery(opts)}`),
  /**
   * What a job would cost for one book (the generate dialog): 404 when the book
   * isn't indexed, 409 kind_unavailable when the Bridge doesn't offer the kind.
   * `brief` skips the costly parts (the earlier books, the ebook look-up): the
   * same shape with `earlier: []` and `source: "audio"`, for what a book has.
   */
  bridgeEstimate: (libraryId: number, path: string, kind: string, brief = false) =>
    request<BridgeEstimate>(
      'GET',
      `/admin/bridge/estimate${bookQuery({ library_id: libraryId, path, kind, brief: brief ? 1 : undefined })}`,
    ),
  /**
   * What a read-along job would take for a book (readalong.align): 404 when the
   * book isn't indexed, 409 kind_unavailable when the Bridge doesn't offer it.
   */
  readalongEstimate: (libraryId: number, path: string) =>
    request<ReadalongEstimate>(
      'GET',
      `/admin/bridge/estimate${bookQuery({ library_id: libraryId, path, kind: 'readalong.align' })}`,
    ),
  /** Queued (cancelled at once) or running (asked to stop); 409 finished once it ended. */
  cancelBridgeJob: (id: number) => request<BridgeJob>('POST', `/admin/bridge/jobs/${id}/cancel`),
  /** One Bridge job by id (404 once it is gone). */
  bridgeJob: (id: number) => request<BridgeJob>('GET', `/admin/bridge/jobs/${id}`),
  /**
   * Queues a job: `created` is false when the open job already queued for that
   * kind, library and path came back instead (200, not 201). 409 kind_unavailable
   * when the paired Bridge doesn't run the kind.
   */
  queueBridgeJob: async (req: BridgeJobRequest) => {
    const res = await send('/admin/bridge/jobs', { method: 'POST', body: req });
    return { job: (await res.json()) as BridgeJob, created: res.status === 201 };
  },

  // Add from LibriVox (Library > LibriVox): the community metadata's LibriVox
  // works and recordings, and the Bridge's librivox.fetch. 404 metadata_off
  // while community metadata is off.
  /** LibriVox works matching q, best first. */
  librivoxSearch: (q: string) =>
    request<{ results: LibrivoxHit[] }>(
      'GET',
      `/admin/librivox/search${bookQuery({ q, limit: 20 })}`,
    ),
  /** A work's LibriVox recordings, where each is on this server and its open job. 404 not_found. */
  librivoxWork: (id: string) =>
    request<LibrivoxWork>('GET', `/admin/librivox/work${bookQuery({ id })}`),
  /**
   * Adds a LibriVox project to a books library: 202 with the queued job (or
   * the open one). 409 in_library, kind_unavailable, library_not_writable;
   * 404 not_found.
   */
  librivoxFetch: (libraryId: number, librivoxId: number) =>
    request<{ job: BridgeJob }>('POST', '/admin/librivox/fetch', {
      library_id: libraryId,
      librivox_id: librivoxId,
    }),

  // Audible (Settings > Bridge's Audible accounts, Settings > Audible): read-only;
  // the accounts are signed in and mapped on the Bridge's own page.
  /**
   * Outside the audible module's toggle (Settings > Modules lists the accounts
   * kept while it is off, server#294); an older server answers 404 module_off then.
   */
  audibleAccounts: () => request<AudibleAccounts>('GET', '/admin/audible/accounts'),
  /** Titles by title, a page at a time (`after` is the previous page's next). */
  audibleTitles: (opts: AudibleTitleQuery = {}) =>
    request<AudibleTitlePage>('GET', `/admin/audible/titles${bookQuery(opts)}`),
  /**
   * Check now: queues the account's poll (`created`), or returns the one already
   * open. 409 account_paused (its library was deleted), 409 kind_unavailable.
   */
  pollAudible: async (account: string) => {
    const res = await send(`/admin/audible/accounts/${encodeURIComponent(account)}/poll`, {
      method: 'POST',
    });
    return { job: (await res.json()) as BridgeJob, created: res.status === 201 };
  },

  /**
   * "Back up" one title: 202 with its queued backup's job, or 200 pending for a
   * preorder (queued once a poll finds it available), either with the title
   * as it is now (absent from an older server). 409 already_matched,
   * import_open, not_available, account_paused, kind_unavailable,
   * library_not_writable.
   */
  backupAudible: (account: string, asin: string) =>
    request<{ pending: boolean; job_id: number | null; title?: AudibleTitle }>(
      'POST',
      `/admin/audible/titles/${encodeURIComponent(account)}/${encodeURIComponent(asin)}/backup`,
    ),
  /**
   * "Back up everything" for an account: how many backups it queued, and how
   * many titles it skipped because their destination no longer validates
   * (`skipped` is absent from a server before it).
   */
  backupAllAudible: (account: string) =>
    request<{ queued: number; skipped?: number }>(
      'POST',
      `/admin/audible/accounts/${encodeURIComponent(account)}/backup-all`,
    ),
  /** "Not this book" and a title's destination; 400 invalid_dest_path. */
  patchAudibleTitle: (account: string, asin: string, patch: AudibleTitlePatch) =>
    request<AudibleTitle>(
      'PATCH',
      `/admin/audible/titles/${encodeURIComponent(account)}/${encodeURIComponent(asin)}`,
      patch,
    ),
  /** An account's policy and template; 400 invalid_policy, invalid_template, invalid_dest_path. */
  patchAudibleAccount: (account: string, patch: AudibleAccountPatch) =>
    request<{ account: AudibleAccount }>(
      'PATCH',
      `/admin/audible/accounts/${encodeURIComponent(account)}`,
      patch,
    ),

  /**
   * Removes an account and its titles from this server (its open poll is
   * cancelled); 204 known or not. Signs nothing out of Amazon: that is the
   * Bridge's Accounts page. Not behind the module's toggle.
   */
  removeAudibleAccount: (account: string) =>
    request<void>('DELETE', `/admin/audible/accounts/${encodeURIComponent(account)}`),

  // Listening import (Settings > Import). The Audiobookshelf token rides in the
  // request body only; the server never stores or returns it.
  /** Connects to Audiobookshelf and lists its accounts (400 invalid_url; 502 abs_unreachable / abs_certificate / abs_certificate_redirect / not_abs / abs_unauthorized). */
  absUsers: (url: string, token: string) =>
    request<{ version: string; users: AbsUser[] }>('POST', '/admin/imports/abs/users', {
      url,
      token,
    }),
  /** Starts fetching each mapped account's history in the background (202; 409 import_running). */
  startAbsImport: (body: {
    url: string;
    token: string;
    users: ImportMapping[];
    cutoff: ImportCutoff;
  }) => request<{ imports: Import[] }>('POST', '/admin/imports/abs', body),
  /** Every import (of one person), newest first. */
  imports: (userId?: number) =>
    request<{ imports: Import[] }>('GET', `/admin/imports${bookQuery({ user_id: userId })}`),
  importDetail: (id: number) => request<ImportDetail>('GET', `/admin/imports/${id}`),
  /** Changes the cutoff of an import in review; answers with it recounted (409 import_not_ready). */
  setImportCutoff: (id: number, cutoff: ImportCutoff) =>
    request<ImportDetail>('PATCH', `/admin/imports/${id}`, { cutoff }),
  /**
   * Writes the history (replacing the person's previous Audiobookshelf import). 409
   * import_not_ready unless in review, or when it has no listening and would only undo
   * the person's applied import (wouldOnlyUndo).
   */
  applyImport: (id: number) => request<Import>('POST', `/admin/imports/${id}/apply`),
  /** Removes what an applied import added and puts progress back (409 import_not_applied). */
  undoImport: (id: number) => request<Import>('POST', `/admin/imports/${id}/undo`),
  /** Discards an import that isn't applied (409 import_applied: undo it first). */
  deleteImport: (id: number) => request<void>('DELETE', `/admin/imports/${id}`),

  /** The audit log, newest first; `before` is the previous page's next_before. */
  audit: (opts: AuditFilter & { before?: number; limit?: number } = {}) =>
    request<AuditPage>('GET', `/admin/audit${bookQuery(opts)}`),

  // The first-run flow (FIRST-RUN-PLAN.md F1): its state, and a folder's preview.
  setup: () => request<SetupEnvelope>('GET', '/admin/setup'),
  /** Merges the fields given into the setup state; answers the whole state. */
  updateSetup: (patch: SetupPatch) => request<SetupState>('PUT', '/admin/setup', patch),
  /** What a folder holds and how its books would be named, read without indexing. */
  previewLibrary: (req: LibraryPreviewRequest) =>
    request<LibraryPreview>('POST', '/admin/setup/preview', req),
  libraries: () => request<{ libraries: AdminLibrary[] }>('GET', '/admin/libraries'),
  createLibrary: (lib: LibraryRequest & { name: string; root: string }) =>
    request<Library>('POST', '/admin/libraries', lib),
  /**
   * Fields left out are kept; a new root or new ignore rules queue a rescan
   * (`job`). A new metadata source re-resolves the books without one:
   * `resolving` when a large library is still at it as this answers.
   */
  updateLibrary: (id: number, lib: LibraryRequest) =>
    request<Library & { job?: Job; resolving?: boolean }>('PATCH', `/admin/libraries/${id}`, lib),
  deleteLibrary: (id: number) => request<void>('DELETE', `/admin/libraries/${id}`),
  reorderLibraries: (ids: number[]) =>
    request<{ libraries: AdminLibrary[] }>('PUT', '/admin/libraries/order', { ids }),
  setFolderOverride: (id: number, path: string, mode: FolderMode | null) =>
    mode
      ? request<unknown>('PUT', `/admin/libraries/${id}/folder-override${pathQuery(path)}`, {
          mode,
        })
      : request<unknown>('DELETE', `/admin/libraries/${id}/folder-override${pathQuery(path)}`),
  /**
   * A folder of a library, as the player browses it (books annotated, overrides
   * shown): 500 entries from `offset`, `next_offset` set when there are more.
   */
  browse: (libraryId: number, path: string, offset = 0, detail = false) =>
    request<FsListing>(
      'GET',
      `/libraries/${libraryId}/fs?${new URLSearchParams({
        path,
        limit: '500',
        ...(detail ? { detail: '1' } : {}),
        ...(offset > 0 ? { offset: String(offset) } : {}),
      }).toString()}`,
    ),
  /** The newest books of a library (covers for its card). */
  recentBooks: (libraryId: number, limit: number) =>
    request<BookPage>('GET', `/libraries/${libraryId}/books?sort=recent&limit=${limit}`),
  /** The server's folders, for choosing a library root (absolute paths; "" = the filesystem root). */
  dirs: (path: string) => request<DirListing>('GET', `/admin/fs/dirs${pathQuery(path)}`),

  users: () => request<{ users: AdminUser[] }>('GET', '/admin/users'),
  user: (id: number) => request<UserDetail>('GET', `/admin/users/${id}`),
  createUser: (u: { username: string; password: string; role: User['role']; email?: string }) =>
    request<AdminUser>('POST', '/admin/users', u),
  updateUser: (id: number, patch: UserPatch) =>
    request<AdminUser>('PATCH', `/admin/users/${id}`, patch),
  /**
   * Deletes a person with the household members in `deleteMembers` (the ones the
   * dialog showed and the admin left ticked); every other member, `keepMembers` and
   * any added since the dialog read the list, becomes an independent account. Always
   * sends `delete_members`, even empty: without it the server deletes every member
   * not kept (server#222). detail=1 asks for the counts recorded by the delete;
   * undefined when a 2xx carries none (a proxy's page).
   */
  deleteUser: (id: number, deleteMembers: readonly number[], keepMembers: readonly number[] = []) =>
    request<{ members_deleted: number; members_kept: number } | undefined>(
      'DELETE',
      `/admin/users/${id}?${new URLSearchParams({
        detail: '1',
        delete_members: deleteMembers.join(','),
        ...(keepMembers.length ? { keep_members: keepMembers.join(',') } : {}),
      }).toString()}`,
    ),
  clearRecovery: (id: number) => request<void>('DELETE', `/admin/users/${id}/recovery`),
  /**
   * Signs a person out of every phone, tablet and browser (their sessions; their API keys
   * and calendar links stay, their household members' made on those devices end), keeping
   * the console's own when it is the admin's account.
   */
  signOutEverywhere: (id: number) =>
    request<{ signed_out: number }>('POST', `/admin/users/${id}/sign-out`),
  /**
   * Ends a person's API keys and their calendar link (server#285); their sessions stay.
   * Answers what it ended.
   */
  endKeys: (id: number) => request<EndedKeys>('POST', `/admin/users/${id}/end-keys`),
  /** Settings > Sign-in: the single sign-on as it runs, and its provider's discovery. */
  oidcStatus: () => request<OIDCStatus>('GET', '/admin/oidc/status'),
  /** People > Waiting sign-ins: the single sign-ons no account took. */
  oidcPending: () => request<OIDCPendingList>('GET', '/admin/oidc/pending'),
  /** Links a waiting sign-in to an existing account (409 not_linkable, user_has_identity, identity_linked). */
  linkPending: (id: number, userId: number) =>
    request<AdminUser>('POST', `/admin/oidc/pending/${id}/link`, { user_id: userId }),
  /** Makes an ordinary account for a waiting sign-in ("" = the server picks the username). */
  createFromPending: (id: number, username: string) =>
    request<AdminUser>('POST', `/admin/oidc/pending/${id}/create`, username ? { username } : {}),
  dismissPending: (id: number) => request<void>('DELETE', `/admin/oidc/pending/${id}`),
  /** Unlinks a person's single sign-on (409 admin_needs_password: a password-less admin's way in). */
  unlinkIdentity: (userId: number, identityId: number) =>
    request<void>('DELETE', `/admin/users/${userId}/identities/${identityId}`),
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
  /** How many books match the filter, with none of the facets counted (`only=total`). */
  bookCount: (filter: BookFilter) =>
    request<{ total: number }>(
      'GET',
      `/admin/books/facets${bookQuery({ ...filter, only: 'total' })}`,
    ).then((r) => r.total),
  /** One field edit over many books, all or nothing (at most BULK_LIMIT). */
  bulkEdit: (books: BookRef[], edit: Omit<BookEditRequest, 'chapters' | 'match'>) =>
    request<{ updated: number }>('POST', '/admin/books/bulk', { books, ...edit }),
  mergeSpellings: (field: MergeField, sources: string[], target: string, libraryId?: number) =>
    request<{ books: MergedBook[] }>('POST', '/admin/books/merge', {
      field,
      sources,
      target,
      library_id: libraryId,
    }),
  /** Puts a merge's books back (its reply sent back) in one transaction. */
  undoMerge: (field: MergeField, target: string, books: MergedBook[]) =>
    request<MergeUndone>('POST', '/admin/books/merge/undo', { field, target, books }),
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
  /**
   * The book page. `warm` (the chapter editor's ask, and only its) has the server
   * start the remuxes of its MP3-in-.mka files not made yet; a plain read starts none.
   */
  adminBook: (libraryId: number, path: string, warm = false) =>
    request<AdminBookDetail>(
      'GET',
      `/admin/libraries/${libraryId}/book${pathQuery(path)}${warm ? '&warm=1' : ''}`,
    ),
  /** Sets or reverts overrides; answers with the updated book page. */
  editBook: (libraryId: number, path: string, edit: BookEditRequest) =>
    request<AdminBookDetail>('PATCH', `/admin/libraries/${libraryId}/book${pathQuery(path)}`, edit),
  /**
   * Replaces the book's chapters with an edited list (whole-book starts) against the
   * files it was made for (`basis`, from the page); answers with the page. 400
   * chapters_invalid names the rule broken in `detail`; 409 chapters_basis: the files
   * changed since.
   */
  saveChapters: (libraryId: number, path: string, edit: ChapterEditRequest) =>
    request<AdminBookDetail>(
      'PUT',
      `/admin/libraries/${libraryId}/book/chapters${pathQuery(path)}`,
      edit,
    ),
  /** Reverts an edited chapter list (applied or set aside); answers with the page. */
  revertChapters: (libraryId: number, path: string) =>
    request<AdminBookDetail>(
      'DELETE',
      `/admin/libraries/${libraryId}/book/chapters${pathQuery(path)}`,
    ),
  /**
   * A token for one book's audio (POST /cast/token: book-scoped, short-lived, ended by
   * signing out), so an <audio> element, which can't send the Authorization header,
   * streams it without the session token in its URL. `editor` asks for the chapter
   * editor's kind, counted apart from the admin's cast tokens: minting one never
   * ends a cast.
   */
  bookMediaToken: (libraryId: number, path: string) =>
    request<{ token: string; expires_at: string }>('POST', '/cast/token', {
      library_id: libraryId,
      path,
      editor: true,
    }),
  /** Checks the book against the community's chapter list, in the background; answers with the page. */
  checkCommunityChapters: (libraryId: number, path: string) =>
    request<AdminBookDetail>(
      'POST',
      `/admin/libraries/${libraryId}/book/community-chapters${pathQuery(path)}`,
    ),
  /** Community works the book might be: by its own facts, or by `q` / an ASIN / an ISBN. */
  matchBook: (libraryId: number, path: string, by: MatchBy = {}) =>
    request<{ candidates: MatchCandidate[] }>(
      'GET',
      `/admin/libraries/${libraryId}/book/match${bookQuery({ path, ...by })}`,
    ),
  matchProposal: (libraryId: number, path: string, work: string, recording: string, by: MatchBy) =>
    request<BookMatchProposal>(
      'GET',
      `/admin/books/match-proposal${bookQuery({ library_id: libraryId, path, work_id: work, recording_id: recording, ...by })}`,
    ),
  /** Bulk matching: the kept runs, newest first, and the marketplace a new one prefers. */
  matchRuns: () => request<MatchRunList>('GET', '/admin/match-runs'),
  /** Starts matching every unmatched book (of one library), or a repick, in the background. */
  startMatchRun: (opts: { library_id?: number; mode?: MatchRunMode }) =>
    request<MatchRun>('POST', '/admin/match-runs', opts),
  matchRun: (id: number) => request<MatchRun>('GET', `/admin/match-runs/${id}`),
  matchRunItems: (id: number, opts: { outcome?: MatchOutcome; after?: number; limit?: number }) =>
    request<MatchRunItemPage>('GET', `/admin/match-runs/${id}/items${bookQuery(opts)}`),
  previewMatchRun: (id: number, body: MatchApplyRequest) =>
    request<MatchRun & { apply_count: number }>('POST', `/admin/match-runs/${id}/preview`, body),
  /** Writes the confident matches (less `exclude`, plus `include`) under `scope`, in the background. */
  applyMatchRun: (id: number, body: MatchApplyRequest) =>
    request<MatchRun>('POST', `/admin/match-runs/${id}/apply`, body),
  cancelMatchRun: (id: number) => request<void>('POST', `/admin/match-runs/${id}/cancel`),
  /** Undoes the community matches of one library (none = every library), runs and all. */
  clearCommunityMatches: (libraryId?: number) =>
    request<ClearedMatches>(
      'DELETE',
      `/admin/community-matches${libraryId ? `?library_id=${libraryId}` : ''}`,
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
  /**
   * Reads a book's files again now: its fresh page, or null when the read was still
   * going at the server's answer (202 still_reading: it goes on and is saved).
   */
  rescanBook: async (libraryId: number, path: string): Promise<AdminBookDetail | null> => {
    const r = await request<AdminBookDetail | { still_reading?: boolean }>(
      'POST',
      `/admin/libraries/${libraryId}/book/rescan${pathQuery(path)}`,
    );
    return r && 'still_reading' in r && r.still_reading ? null : (r as AdminBookDetail);
  },

  // Health and Jobs.
  issues: () => request<IssuesSummary>('GET', '/admin/issues'),
  /**
   * A page of duplicate groups: at most `limit` after `cursor` (the last page's
   * next_cursor). across: also copies in different libraries (?across=1, #165).
   */
  duplicates: (
    opts: {
      library_id?: number;
      ignored?: boolean;
      across?: boolean;
      limit?: number;
      cursor?: string;
    } = {},
  ) =>
    request<DuplicatePage>(
      'GET',
      `/admin/issues/duplicates${bookQuery({ ...opts, across: opts.across ? 1 : undefined })}`,
    ),
  /** The series a library's books spell several ways (the open ones, or with `ignored` those with an ignored book). */
  seriesSpellings: (opts: { library_id?: number; ignored?: boolean } = {}) =>
    request<{ groups: SeriesSpellingGroup[] }>(
      'GET',
      `/admin/issues/series-spellings${bookQuery(opts)}`,
    ),
  /** Stops showing these books under a category (at most BULK_LIMIT). */
  ignoreIssue: (kind: IssueKind, books: BookRef[], groupKey?: string) =>
    request<void>('POST', '/admin/issues/ignore', { kind, books, group_key: groupKey }),
  unignoreIssue: (kind: IssueKind, books: BookRef[], groupKey?: string) =>
    request<void>('DELETE', '/admin/issues/ignore', { kind, books, group_key: groupKey }),
  jobs: () => request<JobsState>('GET', '/admin/jobs'),
  /** Drops a queued scan or stops the running one (it stops before removing anything). */
  cancelJob: (id: number) => request<void>('DELETE', `/admin/jobs/${id}`),
  scanRuns: (opts: { library_id?: number; before?: number; limit?: number } = {}) =>
    request<ScanRunPage>('GET', `/admin/scan-runs${bookQuery(opts)}`),
  scanRun: (id: number) => request<ScanRun>('GET', `/admin/scan-runs/${id}`),

  // Activity: sessions, devices, progress edits and listening stats.
  /** The Activity page for a period ("7d", "30d", "90d", "1y" or a year), in server time. */
  activity: (range: string): Promise<Activity> => activityStats<Activity>({ range }),
  /** A period's listening alone (Year in listening): none of the library and progress steps. */
  listeningActivity: (range: string): Promise<ListeningActivity> =>
    activityStats<ListeningActivity>({ range, parts: 'listening' }),
  /** Listening per day over a period, of everyone or one person (no other stats). */
  listeningDays: (range: string, userId?: number) =>
    request<ListeningDays>('GET', `/admin/listening${bookQuery({ range, user_id: userId })}`),
  /** Who is listening now: one session per device, with chapter and address. */
  liveSessions: () => request<{ sessions: ListeningSession[] }>('GET', '/admin/sessions/live'),
  /** Sessions newest first; the cursor is the previous page's next_before and last start (1-200 a page). */
  sessions: (opts: SessionFilter & SessionCursor & { limit?: number } = {}) =>
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
 * the server names. With `personal`, every copy of a book is its own row carrying
 * its path, tags and description (?include=paths,tags,description&copies=separate):
 * a file for the admin's own scripts, not one to share. Returns the file name.
 */
export function downloadLibraryExport(
  id: number,
  opts: { personal?: boolean } = {},
): Promise<string> {
  const query = opts.personal ? '?include=paths,tags,description&copies=separate' : '';
  // The fallback name keeps the server's "-private" mark (catalog.Filename).
  const fallback = `audiosilo-library-${id}${opts.personal ? '-private' : ''}.json`;
  return download(`/admin/libraries/${id}/export${query}`, fallback);
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
