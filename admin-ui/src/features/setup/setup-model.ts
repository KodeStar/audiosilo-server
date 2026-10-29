import { ApiError } from '@/api/client';
import {
  METADATA_SOURCES,
  PREVIEW_FIELDS,
  SETUP_STEPS,
  type AdminSettings,
  type Library,
  type MetadataSource,
  type PreviewNames,
  type PreviewSample,
  type SettingsPatch,
  type SettingsSection,
  type SetupMark,
  type SetupMarks,
  type SetupRequest,
  type SetupState,
  type SetupStepId,
} from '@/api/types';
import { lockOf, sectionPatch, type SettingId } from '@/features/settings/settings-model';
import { isBookLibrary } from '@/lib/library-kind';
import { absoluteBaseName } from '@/lib/paths';

// The first-run flow (FIRST-RUN-PLAN.md Phase F1): which step comes next, where
// a return visit picks up, what the Overview card lists, and the small rules
// the steps share (a preview's disagreements, locked settings, a reverse proxy).

export function isSetupStep(v: unknown): v is SetupStepId {
  return SETUP_STEPS.includes(v as SetupStepId);
}

/** The step's place in the flow, from 0. */
export function stepIndex(id: SetupStepId): number {
  return SETUP_STEPS.indexOf(id);
}

/**
 * The one rule for the folder step: a books library on the server satisfies
 * it, whatever its mark (it was added here, by config, or by an older setup
 * page); `hasLibraries` is hasBookLibrary's answer. Where the flow opens, what
 * Finish marks and what the Overview card lists all follow it.
 */
export function folderSatisfied(hasLibraries: boolean): boolean {
  return hasLibraries;
}

/** Whether the server has a library of books: a podcast library holds no audiobooks, so it isn't the folder step's. */
export function hasBookLibrary(libraries: readonly Pick<Library, 'kind'>[]): boolean {
  return libraries.some(isBookLibrary);
}

/** What this browser holds for the folder step: its draft, and whether a books library exists. */
export interface FolderFacts {
  hasDraft: boolean;
  hasLibraries: boolean;
}

/**
 * The folder step marked done, yet nothing came of it here: the folder chosen
 * lives only in the draft of the browser that chose it (server#183 keeps it on
 * the server, later), so in another browser, or after the draft was cleared,
 * there's no draft and no library. The step is then not done in this browser
 * and asked again, rather than naming nothing.
 */
export function folderLost(marks: SetupMarks, { hasDraft, hasLibraries }: FolderFacts): boolean {
  return marks.folder === 'done' && !hasDraft && !folderSatisfied(hasLibraries);
}

/** The steps that turn the draft into a library: they need the folder chosen. */
function needsFolder(id: SetupStepId): boolean {
  return id === 'naming' || id === 'metadata';
}

/**
 * Where the flow opens: the first step without a mark (the last step when all
 * have one, so Finish is a click away), or the step `requested` (`?step=`).
 * Either way, a step that needs the folder opens the folder step instead when
 * the folder is lost (folderLost).
 */
export function resumeStep(
  marks: SetupMarks,
  facts: FolderFacts,
  requested?: SetupStepId,
): SetupStepId {
  const first =
    requested ?? SETUP_STEPS.find((id) => !marks[id]) ?? SETUP_STEPS[SETUP_STEPS.length - 1];
  if (needsFolder(first) && folderLost(marks, facts)) return 'folder';
  return first;
}

/**
 * The step after `id`, or null when the flow ends there. While the flow runs
 * that's the next step in order. Once it has finished (a skipped step reopened
 * from the Overview card) it is the next step still skipped, so the admin goes
 * through what's left and then back to Overview; while a draft waits (a folder
 * chosen on that visit), also the steps that turn it into a library, whatever
 * their mark: naming while it has no source, and metadata, which creates it.
 */
export function nextStep(
  id: SetupStepId,
  state: SetupState,
  draft: SetupDraft | null = null,
): SetupStepId | null {
  const after = SETUP_STEPS.slice(stepIndex(id) + 1);
  if (!state.finished) return after[0] ?? null;
  const creates = (s: SetupStepId) =>
    !!draft && (s === 'metadata' || (s === 'naming' && !draft.source));
  return after.find((s) => state.steps[s] === 'skipped' || creates(s)) ?? null;
}

/** The step before `id`, or null on the first. */
export function previousStep(id: SetupStepId): SetupStepId | null {
  return SETUP_STEPS[stepIndex(id) - 1] ?? null;
}

/**
 * The steps passed over and still left to do, in flow order: a skipped folder
 * step isn't, once the server has a library (folderSatisfied).
 */
export function skippedSteps(state: SetupState, hasLibraries: boolean): SetupStepId[] {
  return SETUP_STEPS.filter(
    (id) => state.steps[id] === 'skipped' && !(id === 'folder' && folderSatisfied(hasLibraries)),
  );
}

/**
 * The marks Finish sends: the step left with its own mark, and every step
 * without one (passed over by the stepper, or never reached) as skipped, so
 * the Overview card lists what is left; an unmarked folder step is done once
 * the server has a library (folderSatisfied). `marks` should be the server's
 * current marks (read again just before Finish), so a step another tab
 * recorded isn't overwritten.
 */
export function finishMarks(
  marks: SetupMarks,
  current: SetupStepId,
  mark: SetupMark,
  hasLibraries: boolean,
): SetupMarks {
  const left = SETUP_STEPS.filter((id) => id !== current && !marks[id]);
  const unmarked = (id: SetupStepId): SetupMark =>
    id === 'folder' && folderSatisfied(hasLibraries) ? 'done' : 'skipped';
  return { ...Object.fromEntries(left.map((id) => [id, unmarked(id)])), [current]: mark };
}

/**
 * The Overview's "Finish setting up" card, until dismissed: while the flow is
 * unfinished ("Finish later" left it), and after it while a step is still left
 * to do.
 */
export function showFinishCard(
  state: SetupState | undefined,
  hasLibraries: boolean,
): state is SetupState {
  if (!state || state.dismissed) return false;
  return !state.finished || skippedSteps(state, hasLibraries).length > 0;
}

/**
 * Whether the console sends the admin to the flow: a server that hasn't
 * finished it, once per browser session, from anywhere but the flow itself.
 */
export function shouldRedirect(
  state: SetupState | undefined,
  pathname: string,
  redirectedThisSession: boolean,
): boolean {
  return !!state && !state.finished && !redirectedThisSession && !isSetupPath(pathname);
}

export function isSetupPath(pathname: string): boolean {
  return pathname === '/setup' || pathname === '/setup/';
}

// --- The new library's draft --------------------------------------------------

/**
 * The library the flow will create once its naming source and region are
 * chosen (decision 5): kept in this browser between steps (and a reload) until
 * the metadata step creates it.
 */
export interface SetupDraft {
  root: string;
  name: string;
  /** The naming step's pick; absent while it hasn't been made (the server's default then). */
  source?: MetadataSource;
}

/** Reads a stored draft; anything malformed reads as none. */
export function parseDraft(text: string | null): SetupDraft | null {
  if (!text) return null;
  try {
    const v = JSON.parse(text) as Partial<SetupDraft>;
    if (typeof v.root !== 'string' || !v.root || typeof v.name !== 'string') return null;
    const source = METADATA_SOURCES.includes(v.source as MetadataSource)
      ? (v.source as MetadataSource)
      : undefined;
    return { root: v.root, name: v.name, ...(source ? { source } : {}) };
  } catch {
    return null;
  }
}

// --- The naming preview -------------------------------------------------------

/** How one field of a sample reads from each side. */
export interface FieldCompare {
  tags: string;
  path: string;
  /** Both sides have a value, and they don't agree. */
  differs: boolean;
}

const norm = (s: string) => s.trim().replace(/\s+/g, ' ').toLowerCase();

/** One field of a sample, from tags and from folders, and whether the two disagree. */
export function compareField(sample: PreviewSample, field: keyof PreviewNames): FieldCompare {
  const tags = sample.tags[field].trim();
  const path = sample.path[field].trim();
  return { tags, path, differs: tags !== '' && path !== '' && norm(tags) !== norm(path) };
}

/** How many fields of the samples the two sides disagree on. */
export function disagreements(samples: readonly PreviewSample[]): number {
  return samples.reduce(
    (n, s) => n + PREVIEW_FIELDS.filter((f) => compareField(s, f).differs).length,
    0,
  );
}

// --- The library's default name -------------------------------------------------

/** The name a new library gets from its folder: the folder's name, capitalised ("/audiobooks": "Audiobooks"). */
export function defaultLibraryName(root: string): string {
  const base = absoluteBaseName(root.trim());
  return base.charAt(0).toLocaleUpperCase() + base.slice(1);
}

// --- Folder check errors -------------------------------------------------------

/** The refusals the folder step words itself (the preview's and library create's codes). */
const FOLDER_ERRORS = ['path_not_absolute', 'folder_unreadable', 'root_overlaps'] as const;

/** The i18n key of a refused folder, or null when the server's own sentence should show. */
export function folderErrorKey(err: unknown): string | null {
  if (!(err instanceof ApiError) || !err.code) return null;
  return (FOLDER_ERRORS as readonly string[]).includes(err.code)
    ? `setup.folder.error.${err.code}`
    : null;
}

// --- Settings the flow writes -------------------------------------------------

/** Whether the console may change a setting (not set by the environment or the desktop app). */
export function editable(settings: AdminSettings, id: SettingId): boolean {
  return !lockOf(settings, id);
}

/**
 * One section's PATCH of the draft's changed fields, leaving out every locked
 * setting (the server would refuse it with setting_locked); undefined when
 * nothing is left to send.
 */
export function unlockedPatch<S extends SettingsSection>(
  settings: AdminSettings,
  section: S,
  draft: Partial<AdminSettings[S]>,
): SettingsPatch | undefined {
  const open = Object.fromEntries(
    Object.entries(draft).filter(([name]) => editable(settings, `${section}.${name}` as SettingId)),
  ) as Partial<AdminSettings[S]>;
  return sectionPatch(section, settings[section], open);
}

/** Several sections' patches as one PATCH body (undefined when all are). */
export function mergePatches(...patches: (SettingsPatch | undefined)[]): SettingsPatch | undefined {
  const parts = patches.filter((p): p is SettingsPatch => p !== undefined);
  return parts.length ? (Object.assign({}, ...parts) as SettingsPatch) : undefined;
}

// --- How people reach the server ----------------------------------------------

/** The address the server saw this request at ("http://nas:8080"). */
export function seenAs(req: SetupRequest): string {
  return `${req.scheme}://${req.host}`;
}

/** The server saw another address than the browser shows: something in between rewrote it. */
export function addressMismatch(req: SetupRequest, origin: string): boolean {
  return seenAs(req).toLowerCase() !== origin.toLowerCase();
}

/**
 * The reverse proxy to offer for trusted_proxies: the request came through one
 * whose address isn't trusted yet (nor already added to the draft).
 */
export function proxyToTrust(req: SetupRequest, trusted: readonly string[]): string | null {
  const peer = req.peer;
  if (!req.proxied || req.peer_trusted || !peer) return null;
  // The server stores a bare address as its one-address range (fixProxies).
  return trusted.some((t) => t === peer || t === `${peer}/32` || t === `${peer}/128`) ? null : peer;
}

/**
 * Whether a request's Host ("host" or "host:port", an IPv6 address in
 * brackets) names the server on a home network, by the server's own rule
 * (config.HomeNetworkHost): a private IP (RFC 1918, IPv6 fc00::/7) or a
 * link-local one, a name ending in .local, .lan, .home.arpa or .internal, or a
 * single-label name. Loopback (127.0.0.0/8, ::1, localhost) is not: no other
 * device can reach it. Nor is carrier-grade NAT space (100.64.0.0/10).
 */
export function isHomeHost(hostport: string): boolean {
  const host = hostOf(hostport);
  if (host === null) return false;
  const v4 = ipv4(host);
  if (v4) return homeIPv4(v4);
  if (host.includes(':')) {
    const v6 = ipv6(host);
    if (!v6) return false;
    // An IPv4-mapped address (::ffff:a.b.c.d) is judged as its IPv4 one.
    if (v6.slice(0, 5).every((n) => n === 0) && v6[5] === 0xffff) {
      return homeIPv4([v6[6] >> 8, v6[6] & 0xff, v6[7] >> 8, v6[7] & 0xff]);
    }
    return (v6[0] & 0xfe00) === 0xfc00 || (v6[0] & 0xffc0) === 0xfe80;
  }
  if (hostport.startsWith('[')) return false; // brackets hold an address, never a name
  const name = host.toLowerCase().replace(/\.$/, '');
  if (!isHostName(name) || name === 'localhost') return false;
  if (['.local', '.lan', '.home.arpa', '.internal'].some((x) => name.endsWith(x))) return true;
  return !name.includes('.');
}

/** A loopback Host (localhost, 127.0.0.0/8, ::1): an address only this machine reaches. */
function isLoopbackHost(hostport: string): boolean {
  const host = hostOf(hostport);
  if (host === null) return false;
  const name = host.toLowerCase().replace(/\.$/, '');
  if (name === 'localhost' || name.endsWith('.localhost')) return true;
  const v4 = ipv4(host);
  if (v4) return v4[0] === 127;
  const v6 = host.includes(':') ? ipv6(host) : null;
  return !!v6 && v6.slice(0, 7).every((n) => n === 0) && v6[7] === 1;
}

/**
 * Which of the server's addresses the reach step offers the browser's own
 * address for, the likelier first: a home-network address suits lan_url (and
 * may still be the only one there is), anything else public_url; a loopback
 * one neither, since no other device can use it.
 */
export function reachOffers(hostport: string): ('lan' | 'public')[] {
  if (isLoopbackHost(hostport)) return [];
  return isHomeHost(hostport) ? ['lan', 'public'] : ['public'];
}

/**
 * A server address as typed into the reach step, the way the server will
 * store it (config httpURL): trimmed, without a trailing slash, and with
 * `scheme`:// added when it has none ("192.168.1.20:8080" ->
 * "http://192.168.1.20:8080", as the placeholder shows). "" stays "" (no
 * address). Null when it still isn't a plain http(s) address (another scheme,
 * a mistyped one, no host, a ?, # or user name), so the field can say so
 * before the server is asked.
 */
export function normalizeAddress(raw: string, scheme: 'http' | 'https'): string | null {
  const s = raw.trim();
  if (s === '') return '';
  // The scheme is judged before trailing slashes go, so "http://" stays no address.
  const hasScheme = /^[a-z][a-z0-9+.-]*:\/\//i.test(s);
  // A scheme mistyped ("http:/nas", "http//nas", "ftp:nas") would read as a
  // host named after it; a port after the host ("nas.local:8080") is no scheme.
  if (!hasScheme && (/^[a-z][a-z0-9+.-]*:(?!\d)/i.test(s) || /^https?\/\//i.test(s))) return null;
  const full = (hasScheme ? s : `${scheme}://${s}`).replace(/\/+$/, '');
  let u: URL;
  try {
    u = new URL(full);
  } catch {
    return null;
  }
  if ((u.protocol !== 'http:' && u.protocol !== 'https:') || !u.hostname) return null;
  if (u.search || u.hash || u.username || u.password || full.includes('?') || full.includes('#')) {
    return null;
  }
  return full;
}

/** Whether an address ("http://127.0.0.1:8080/x") names a loopback host; false for one that isn't a URL. */
export function isLoopbackUrl(address: string): boolean {
  try {
    return isLoopbackHost(new URL(address).host);
  } catch {
    return false;
  }
}

/**
 * The link the phone step's QR encodes: the pairing's own (web_url). When that
 * names a loopback address (the console opened at localhost or 127.0.0.1, with
 * no public address), a phone can't open it: the same link at the server's
 * home address instead when there is one (`via: 'home'`), else the loopback
 * link as it is (`via: 'loopback'`, the step says so and points to the reach
 * step).
 */
export function phoneLink(
  webUrl: string,
  home: string | undefined,
): { url: string; via: 'pairing' | 'home' | 'loopback' } {
  if (!isLoopbackUrl(webUrl)) return { url: webUrl, via: 'pairing' };
  if (home && !isLoopbackUrl(home)) {
    try {
      const link = new URL(webUrl);
      const base = new URL(home);
      const prefix = base.pathname.replace(/\/+$/, '');
      return { url: `${base.origin}${prefix}${link.pathname}${link.search}`, via: 'home' };
    } catch {
      // a malformed home address: fall through to the warning
    }
  }
  return { url: webUrl, via: 'loopback' };
}

/** The host of a Host header without its port or brackets; null for a malformed one. */
function hostOf(hostport: string): string | null {
  const bracketed = /^\[([^\]]*)\](?::(\d+))?$/.exec(hostport);
  if (bracketed) return bracketed[2] !== undefined && !isPort(bracketed[2]) ? null : bracketed[1];
  if (hostport.startsWith('[')) return null;
  const colons = hostport.split(':').length - 1;
  if (colons === 1) {
    const [host, port] = hostport.split(':');
    return isPort(port) ? host : null;
  }
  return hostport; // no port, or a bare IPv6 address
}

function isPort(s: string): boolean {
  return /^\d{1,5}$/.test(s) && Number(s) > 0 && Number(s) <= 65535;
}

function ipv4(s: string): number[] | null {
  const m = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(s);
  if (!m) return null;
  const parts = m.slice(1).map(Number);
  // Go's netip refuses a leading zero ("010") as well as an octet over 255.
  if (m.slice(1).some((p) => p.length > 1 && p.startsWith('0'))) return null;
  return parts.every((n) => n <= 255) ? parts : null;
}

/** An IPv6 address as its eight 16-bit groups (an IPv4 tail allowed); null if it isn't one. */
function ipv6(addr: string): number[] | null {
  let s = addr.replace(/%.*$/, ''); // a zone ("fe80::1%en0")
  const last = s.slice(s.lastIndexOf(':') + 1);
  if (last.includes('.')) {
    // "::ffff:192.168.1.1": the IPv4 tail as its two groups.
    const v4 = ipv4(last);
    if (!v4) return null;
    const hex = (hi: number, lo: number) => ((hi << 8) | lo).toString(16);
    s = `${s.slice(0, s.length - last.length)}${hex(v4[0], v4[1])}:${hex(v4[2], v4[3])}`;
  }
  const halves = s.split('::');
  if (halves.length > 2) return null;
  const groups = (h: string) => (h === '' ? [] : h.split(':'));
  const head = groups(halves[0]);
  const rest = halves.length === 2 ? groups(halves[1]) : [];
  if ([...head, ...rest].some((g) => !/^[0-9a-f]{1,4}$/i.test(g))) return null;
  const given = head.length + rest.length;
  if (halves.length === 1 ? given !== 8 : given > 7) return null;
  return [...head, ...Array<string>(8 - given).fill('0'), ...rest].map((g) => parseInt(g, 16));
}

function homeIPv4([a, b]: number[]): boolean {
  return (
    a === 10 ||
    (a === 172 && b >= 16 && b <= 31) ||
    (a === 192 && b === 168) ||
    (a === 169 && b === 254)
  );
}

/** A DNS name of letters, digits and hyphens in non-empty dot-separated labels (lowercase already). */
function isHostName(s: string): boolean {
  if (s === '' || s.length > 253) return false;
  return s.split('.').every((l) => /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/.test(l));
}
