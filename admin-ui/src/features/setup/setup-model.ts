import { ApiError } from '@/api/client';
import {
  METADATA_SOURCES,
  PREVIEW_FIELDS,
  SETUP_STEPS,
  type AdminSettings,
  type MetadataSource,
  type PreviewNames,
  type PreviewSample,
  type SettingsPatch,
  type SettingsSection,
  type SetupMarks,
  type SetupRequest,
  type SetupState,
  type SetupStepId,
} from '@/api/types';
import { lockOf, sectionPatch, type SettingId } from '@/features/settings/settings-model';

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
 * Where the flow opens: the first step without a mark (the last step when all
 * have one, so Finish is a click away). A folder chosen on another visit lives
 * only in that browser's draft, so when that draft is gone and the server has
 * no library yet, the folder step is asked again rather than naming nothing.
 */
export function resumeStep(
  marks: SetupMarks,
  { hasDraft, hasLibraries }: { hasDraft: boolean; hasLibraries: boolean },
): SetupStepId {
  const first = SETUP_STEPS.find((id) => !marks[id]) ?? SETUP_STEPS[SETUP_STEPS.length - 1];
  const beforeCreate = first === 'naming' || first === 'metadata';
  if (beforeCreate && marks.folder === 'done' && !hasDraft && !hasLibraries) return 'folder';
  return first;
}

/**
 * The step after `id`, or null when the flow ends there. While the flow runs
 * that's the next step in order. Once it has finished (a skipped step reopened
 * from the Overview card) it is the next step still skipped, so the admin goes
 * through what's left and then back to Overview.
 */
export function nextStep(id: SetupStepId, state: SetupState): SetupStepId | null {
  const after = SETUP_STEPS.slice(stepIndex(id) + 1);
  if (!state.finished) return after[0] ?? null;
  return after.find((s) => state.steps[s] === 'skipped') ?? null;
}

/** The step before `id`, or null on the first. */
export function previousStep(id: SetupStepId): SetupStepId | null {
  return SETUP_STEPS[stepIndex(id) - 1] ?? null;
}

/** The steps passed over, in flow order. */
export function skippedSteps(state: SetupState): SetupStepId[] {
  return SETUP_STEPS.filter((id) => state.steps[id] === 'skipped');
}

/** The Overview's "Finish setting up" card: after the flow, while a step is still skipped, until dismissed. */
export function showFinishCard(state: SetupState | undefined): state is SetupState {
  return !!state && state.finished && !state.dismissed && skippedSteps(state).length > 0;
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
  if (!req.proxied || req.peer_trusted || !req.peer) return null;
  return trusted.includes(req.peer) ? null : req.peer;
}

/**
 * Whether a host is a home-network one (a private or loopback address, or a
 * .local/.lan/.home name), so the browser's address suits lan_url rather than
 * public_url.
 */
export function isHomeHost(host: string): boolean {
  const h = host
    .replace(/:\d+$/, '')
    .replace(/^\[|\]$/g, '')
    .toLowerCase();
  if (h === 'localhost' || h === '::1') return true;
  // IPv6: unique-local (fc00::/7) and link-local (fe80::/10) are home addresses.
  if (h.includes(':')) return /^f[cd]/.test(h) || /^fe[89ab]/.test(h);
  // A single-label name ("nas") only resolves on the home network.
  if (!h.includes('.')) return true;
  if (/\.(local|lan|home|internal|home\.arpa)$/.test(h)) return true;
  const ip = h.split('.').map(Number);
  if (ip.length !== 4 || ip.some((n) => !Number.isInteger(n) || n < 0 || n > 255)) return false;
  const [a, b] = ip;
  return a === 10 || a === 127 || (a === 192 && b === 168) || (a === 172 && b >= 16 && b <= 31);
}

// --- Adding a Bridge ------------------------------------------------------------

/**
 * The Bridge's service for the server's compose file, from audiosilo-bridge's
 * compose.example.yaml. It mounts the library folder read-write at the
 * server's own path (`root`), so the Bridge needs no mapping. The agent
 * container (characters and recaps) is in the full example.
 */
export function bridgeCompose(root: string): string {
  const at = root.trim() || '/audiobooks';
  return `  bridge:
    image: ghcr.io/kodestar/audiosilo-bridge:latest
    restart: unless-stopped
    depends_on: [audiosilo]
    environment:
      PUID: "1000"
      PGID: "1000"
      # The server as the Bridge reaches it on this network.
      AUDIOSILO_BRIDGE_SERVER_URL: http://audiosilo:8080
      # The Bridge's page as your LAN reaches it (the console links to it).
      AUDIOSILO_BRIDGE_PUBLIC_URL: http://192.168.1.10:8091
      AUDIOSILO_BRIDGE_AGENT_STAGING_DIR: /staging
    ports:
      - "8091:8091"
    volumes:
      - ./bridge-data:/bridge-data
      - /path/to/audiobooks:${at}:rw
      - staging:/staging

# And under the file's top-level volumes:
#   staging: {}`;
}
