import { ApiError } from '@/api/client';
import type { PreviewSample, SetupRequest, SetupState } from '@/api/types';
import { settingsWith } from '@/test/fixtures';
import {
  addressMismatch,
  compareField,
  disagreements,
  finishMarks,
  folderErrorKey,
  isHomeHost,
  isLoopbackUrl,
  hasBookLibrary,
  normalizeAddress,
  phoneLink,
  isSetupStep,
  mergePatches,
  nextStep,
  parseDraft,
  previousStep,
  proxyToTrust,
  reachOffers,
  resumeStep,
  shouldRedirect,
  showFinishCard,
  skippedSteps,
  unlockedPatch,
} from './setup-model';

const state = (over: Partial<SetupState> = {}): SetupState => ({
  finished: false,
  dismissed: false,
  steps: {},
  ...over,
});

const fresh = { hasDraft: false, hasLibraries: false };

describe('resumeStep', () => {
  it('opens a fresh server at the folder step', () => {
    expect(resumeStep({}, fresh)).toBe('folder');
  });

  it('picks up at the first step without a mark', () => {
    expect(resumeStep({ folder: 'done' }, { ...fresh, hasDraft: true })).toBe('naming');
    expect(
      resumeStep({ folder: 'done', naming: 'skipped', metadata: 'done', reach: 'done' }, fresh),
    ).toBe('abs');
  });

  it('asks for the folder again when its draft is gone and no library exists', () => {
    expect(resumeStep({ folder: 'done' }, fresh)).toBe('folder');
    expect(resumeStep({ folder: 'done', naming: 'done' }, fresh)).toBe('folder');
  });

  it('goes on to naming when the server already has libraries (config-declared ones)', () => {
    expect(resumeStep({ folder: 'done' }, { hasDraft: false, hasLibraries: true })).toBe('naming');
    expect(
      resumeStep({ folder: 'done', naming: 'done' }, { hasDraft: false, hasLibraries: true }),
    ).toBe('metadata');
  });

  it('a skipped folder never sends the admin back to it', () => {
    expect(resumeStep({ folder: 'skipped' }, fresh)).toBe('naming');
  });

  it('opens at the last step once every step has a mark', () => {
    const all = {
      folder: 'done',
      naming: 'done',
      metadata: 'done',
      reach: 'done',
      abs: 'skipped',
      backups: 'done',
      phone: 'done',
      bridge: 'skipped',
    } as const;
    expect(resumeStep(all, fresh)).toBe('bridge');
  });
});

describe('nextStep and previousStep', () => {
  it('walks the steps in order while the flow runs, and ends after the Bridge', () => {
    expect(nextStep('folder', state())).toBe('naming');
    expect(nextStep('phone', state())).toBe('bridge');
    expect(nextStep('bridge', state())).toBeNull();
    expect(previousStep('naming')).toBe('folder');
    expect(previousStep('folder')).toBeNull();
  });

  it('after the flow, goes through the steps still skipped, then back to Overview', () => {
    const done = state({
      finished: true,
      steps: { naming: 'skipped', reach: 'done', abs: 'skipped', bridge: 'skipped' },
    });
    expect(nextStep('naming', done)).toBe('abs');
    expect(nextStep('abs', done)).toBe('bridge');
    expect(nextStep('bridge', done)).toBeNull();
    expect(nextStep('phone', state({ finished: true, steps: {} }))).toBeNull();
  });

  it('after the flow, a folder chosen again goes on to the steps that add it', () => {
    const done = state({
      finished: true,
      steps: { folder: 'skipped', naming: 'done', metadata: 'done', bridge: 'skipped' },
    });
    const draft = { root: '/audiobooks', name: 'Audiobooks' };
    expect(nextStep('folder', done, draft)).toBe('naming');
    expect(nextStep('folder', done, { ...draft, source: 'path' })).toBe('metadata');
    expect(nextStep('naming', done, { ...draft, source: 'tags' })).toBe('metadata');
    expect(nextStep('metadata', done, null)).toBe('bridge');
    expect(nextStep('folder', done)).toBe('bridge');
  });

  it('knows the step ids', () => {
    expect(isSetupStep('reach')).toBe(true);
    expect(isSetupStep('demo')).toBe(false);
    expect(isSetupStep(undefined)).toBe(false);
  });
});

describe('the Finish setting up card', () => {
  const skipped = { folder: 'done', abs: 'skipped', bridge: 'skipped' } as const;

  it('Finish marks every step without a mark as skipped, so the card lists them', () => {
    const marks = { folder: 'skipped', naming: 'done', reach: 'done' } as const;
    const sent = finishMarks(marks, 'bridge', 'done', false);
    expect(sent).toEqual({
      metadata: 'skipped',
      abs: 'skipped',
      backups: 'skipped',
      phone: 'skipped',
      bridge: 'done',
    });
    const after = state({ finished: true, steps: { ...marks, ...sent } });
    expect(skippedSteps(after, false)).toEqual(['folder', 'metadata', 'abs', 'backups', 'phone']);
    // A stepper jump straight to the end leaves even the folder on the card.
    expect(
      skippedSteps(
        state({ finished: true, steps: finishMarks({}, 'bridge', 'skipped', false) }),
        false,
      ),
    ).toEqual(['folder', 'naming', 'metadata', 'reach', 'abs', 'backups', 'phone', 'bridge']);
  });

  it('a library on the server satisfies the folder step: Finish marks it done, the card leaves it out', () => {
    expect(finishMarks({}, 'bridge', 'done', true).folder).toBe('done');
    // Its own mark stands; only an unmarked step is filled in.
    expect(finishMarks({ folder: 'skipped' }, 'bridge', 'done', true).folder).toBeUndefined();
    const s = state({ finished: true, steps: { folder: 'skipped', abs: 'skipped' } });
    expect(skippedSteps(s, true)).toEqual(['abs']);
    expect(skippedSteps(s, false)).toEqual(['folder', 'abs']);
    const onlyFolder = state({ finished: true, steps: { folder: 'skipped' } });
    expect(showFinishCard(onlyFolder, true)).toBe(false);
    expect(showFinishCard(onlyFolder, false)).toBe(true);
  });

  it('shows after the flow while a step is skipped, in flow order', () => {
    const s = state({ finished: true, steps: { bridge: 'skipped', abs: 'skipped' } });
    expect(showFinishCard(s, true)).toBe(true);
    expect(skippedSteps(s, true)).toEqual(['abs', 'bridge']);
  });

  it('shows while the flow is unfinished ("Finish later"), so there is a way back', () => {
    expect(showFinishCard(state(), true)).toBe(true);
    expect(showFinishCard(state({ steps: skipped }), true)).toBe(true);
  });

  it('stays away once dismissed, with nothing skipped, or unloaded', () => {
    expect(showFinishCard(state({ dismissed: true }), true)).toBe(false);
    expect(showFinishCard(state({ finished: true, dismissed: true, steps: skipped }), true)).toBe(
      false,
    );
    expect(showFinishCard(state({ finished: true, steps: { folder: 'done' } }), true)).toBe(false);
    expect(showFinishCard(undefined, true)).toBe(false);
  });
});

describe('shouldRedirect', () => {
  it('sends an unfinished server to the flow once a session, from anywhere else', () => {
    expect(shouldRedirect(state(), '/', false)).toBe(true);
    expect(shouldRedirect(state(), '/library/libraries', false)).toBe(true);
  });

  it('not from the flow itself, not twice, not once finished, not before it loads', () => {
    expect(shouldRedirect(state(), '/setup', false)).toBe(false);
    expect(shouldRedirect(state(), '/', true)).toBe(false);
    expect(shouldRedirect(state({ finished: true }), '/', false)).toBe(false);
    expect(shouldRedirect(undefined, '/', false)).toBe(false);
  });
});

describe('parseDraft', () => {
  it('reads a stored draft', () => {
    expect(parseDraft('{"root":"/audiobooks","name":"Audiobooks","source":"path"}')).toEqual({
      root: '/audiobooks',
      name: 'Audiobooks',
      source: 'path',
    });
  });

  it('drops an unknown source and refuses anything malformed', () => {
    expect(parseDraft('{"root":"/a","name":"A","source":"abs"}')).toEqual({
      root: '/a',
      name: 'A',
    });
    expect(parseDraft('{"name":"A"}')).toBeNull();
    expect(parseDraft('not json')).toBeNull();
    expect(parseDraft(null)).toBeNull();
  });
});

describe('the naming preview', () => {
  const names = (o: Partial<PreviewSample['tags']> = {}) => ({
    title: '',
    author: '',
    narrator: '',
    series: '',
    position: '',
    ...o,
  });
  const sample: PreviewSample = {
    rel_path: 'Pratchett/Discworld/04 - Mort',
    tags: names({ title: 'Mort', author: 'Terry  Pratchett', series: 'Discworld' }),
    path: names({
      title: '04 - Mort',
      author: 'terry pratchett',
      series: 'Discworld',
      position: '4',
    }),
    abs: false,
  };

  it('marks a field the two sides name differently, ignoring case and spacing', () => {
    expect(compareField(sample, 'title')).toEqual({
      tags: 'Mort',
      path: '04 - Mort',
      differs: true,
    });
    expect(compareField(sample, 'author').differs).toBe(false);
    expect(compareField(sample, 'series').differs).toBe(false);
  });

  it('a blank on one side is a blank, not a disagreement', () => {
    expect(compareField(sample, 'position')).toEqual({ tags: '', path: '4', differs: false });
    expect(compareField(sample, 'narrator').differs).toBe(false);
  });

  it('counts the disagreements across the samples', () => {
    expect(disagreements([sample, sample])).toBe(2);
    expect(disagreements([])).toBe(0);
  });
});

describe('folderErrorKey', () => {
  it('words the folder refusals itself', () => {
    for (const code of ['path_not_absolute', 'folder_unreadable', 'root_overlaps']) {
      expect(folderErrorKey(new ApiError(400, 'x', code))).toBe(`setup.folder.error.${code}`);
    }
  });

  it("leaves anything else to the server's sentence", () => {
    expect(folderErrorKey(new ApiError(500, 'boom'))).toBeNull();
    expect(folderErrorKey(new ApiError(409, 'taken', 'name_taken'))).toBeNull();
    expect(folderErrorKey(new Error('network'))).toBeNull();
  });
});

describe('locked settings', () => {
  const settings = settingsWith({
    locked: { 'general.public_url': 'AUDIOSILO_PUBLIC_URL' },
  });

  it('leaves a locked setting out of the patch', () => {
    expect(
      unlockedPatch(settings, 'general', {
        public_url: 'https://x.example',
        lan_url: 'http://nas:8080',
      }),
    ).toEqual({ general: { lan_url: 'http://nas:8080' } });
  });

  it('sends nothing when only locked or unchanged settings differ', () => {
    expect(unlockedPatch(settings, 'general', { public_url: 'https://x.example' })).toBeUndefined();
    expect(unlockedPatch(settings, 'general', { lan_url: '' })).toBeUndefined();
  });

  it('merges several sections into one PATCH', () => {
    expect(
      mergePatches({ general: { lan_url: 'a' } }, undefined, { network: { tls_home_http: false } }),
    ).toEqual({ general: { lan_url: 'a' }, network: { tls_home_http: false } });
    expect(mergePatches(undefined, undefined)).toBeUndefined();
  });
});

describe('how people reach the server', () => {
  const req = (over: Partial<SetupRequest> = {}): SetupRequest => ({
    host: 'nas:8080',
    scheme: 'http',
    proxied: false,
    peer_trusted: false,
    ...over,
  });

  it('offers an untrusted proxy for trusted_proxies, once', () => {
    const behind = req({ proxied: true, peer: '172.18.0.2' });
    expect(proxyToTrust(behind, [])).toBe('172.18.0.2');
    expect(proxyToTrust(behind, ['172.18.0.2'])).toBeNull();
    expect(proxyToTrust(behind, ['172.18.0.2/32'])).toBeNull();
    expect(proxyToTrust({ ...behind, peer_trusted: true }, [])).toBeNull();
    expect(proxyToTrust(req(), [])).toBeNull();
  });

  it('notices when the server saw another address than the browser shows', () => {
    expect(addressMismatch(req(), 'http://nas:8080')).toBe(false);
    expect(addressMismatch(req(), 'https://books.example.com')).toBe(true);
  });

  // The cases of the server's TestHomeNetworkHost (internal/config), so the
  // console offers lan_url for exactly the hosts the server derives one from.
  it.each([
    ['10.0.0.5', true],
    ['10.0.0.5:8080', true],
    ['172.16.4.2:8080', true],
    ['172.31.255.255', true],
    ['192.168.1.20:8080', true],
    ['192.168.1.20', true],
    ['172.32.0.1:8080', false],
    ['192.169.1.1', false],
    ['100.64.0.1:8080', false],
    ['100.127.255.254', false],
    ['169.254.10.20:8080', true],
    ['[fe80::1]:8080', true],
    ['[fe80::1%25en0]:8080', true],
    ['[fd12:3456::20]:8080', true],
    ['[fd12:3456::20]', true],
    ['fd12:3456::20', true],
    ['[fc00::1]:443', true],
    ['[::ffff:192.168.1.20]:8080', true],
    ['[::ffff:8.8.8.8]:8080', false],
    ['127.0.0.1:8080', false],
    ['127.0.0.1', false],
    ['127.8.9.10', false],
    ['[::1]:8080', false],
    ['[::1]', false],
    ['::1', false],
    ['0.0.0.0:8080', false],
    ['localhost:8080', false],
    ['localhost', false],
    ['LOCALHOST', false],
    ['app.localhost:8080', false],
    ['8.8.8.8', false],
    ['203.0.113.9:8080', false],
    ['[2001:db8::1]:8080', false],
    ['books.example.com', false],
    ['books.example.com:8443', false],
    ['nas.local:8080', true],
    ['nas.local', true],
    ['NAS.Local.', true],
    ['books.lan', true],
    ['server.home.arpa:8080', true],
    ['nas.internal', true],
    ['nas.internal:8080', true],
    ['internal', true],
    ['foo.internal.example.com', false],
    ['nas.home', false],
    ['nas', true],
    ['nas:8080', true],
    ['my-nas:8080', true],
    ['', false],
    [':8080', false],
    ['nas:0', false],
    ['nas:99999', false],
    ['nas:http', false],
    ['bad_name:8080', false],
    ['-nas:8080', false],
    ['nas..local', false],
    ['[nas]:8080', false],
  ])('isHomeHost(%j) is %s, as the server judges it', (host, want) => {
    expect(isHomeHost(host)).toBe(want);
  });

  it('never offers a loopback address, and offers a home one as the home address first', () => {
    expect(reachOffers('localhost:8080')).toEqual([]);
    expect(reachOffers('127.0.0.1:8080')).toEqual([]);
    expect(reachOffers('[::1]:8080')).toEqual([]);
    expect(reachOffers('192.168.1.20:8080')).toEqual(['lan', 'public']);
    expect(reachOffers('nas.local')).toEqual(['lan', 'public']);
    expect(reachOffers('books.example.com')).toEqual(['public']);
    expect(reachOffers('100.64.0.1:8080')).toEqual(['public']);
  });
});

describe('hasBookLibrary', () => {
  it('counts a books library, not a podcast one, for the folder step', () => {
    expect(hasBookLibrary([])).toBe(false);
    expect(hasBookLibrary([{ kind: 'podcasts' }])).toBe(false);
    expect(hasBookLibrary([{ kind: 'podcasts' }, { kind: 'books' }])).toBe(true);
  });
});

describe('normalizeAddress', () => {
  it('adds the scheme left off, as the placeholders show', () => {
    expect(normalizeAddress('books.example.com', 'https')).toBe('https://books.example.com');
    expect(normalizeAddress(' 192.168.1.20:8080/ ', 'http')).toBe('http://192.168.1.20:8080');
    expect(normalizeAddress('http://nas.local:8080', 'https')).toBe('http://nas.local:8080');
    expect(normalizeAddress('', 'http')).toBe('');
  });

  it('refuses what the server would: another scheme, a query, a user name, no host', () => {
    expect(normalizeAddress('ftp://books.example.com', 'https')).toBeNull();
    expect(normalizeAddress('https://books.example.com/?x=1', 'https')).toBeNull();
    expect(normalizeAddress('https://me@books.example.com', 'https')).toBeNull();
    expect(normalizeAddress('http://', 'http')).toBeNull();
  });

  it('refuses a mistyped scheme rather than reading it as the host', () => {
    expect(normalizeAddress('http:/192.168.1.20:8080', 'http')).toBeNull();
    expect(normalizeAddress('http//192.168.1.20:8080', 'http')).toBeNull();
    expect(normalizeAddress('https:/books.example.com', 'https')).toBeNull();
    expect(normalizeAddress('ftp:/books.example.com', 'https')).toBeNull();
    // A host with a port is no scheme.
    expect(normalizeAddress('nas.local:8080', 'http')).toBe('http://nas.local:8080');
    expect(normalizeAddress('localhost:8080/', 'http')).toBe('http://localhost:8080');
  });
});

describe('phoneLink', () => {
  const link = 'http://127.0.0.1:19297/web/connect?token=abc&sid=s1';

  it('keeps a link a phone can open', () => {
    const ok = 'https://books.example.com/web/connect?token=abc';
    expect(phoneLink(ok, 'http://192.168.1.20:8080')).toEqual({ url: ok, via: 'pairing' });
  });

  it('moves a loopback link to the home address', () => {
    expect(phoneLink(link, 'http://192.168.1.20:8080/')).toEqual({
      url: 'http://192.168.1.20:8080/web/connect?token=abc&sid=s1',
      via: 'home',
    });
  });

  it('says so when there is no other address', () => {
    expect(phoneLink(link, undefined)).toEqual({ url: link, via: 'loopback' });
    expect(phoneLink(link, 'http://localhost:8080')).toEqual({ url: link, via: 'loopback' });
  });

  it('knows a loopback address', () => {
    expect(isLoopbackUrl('http://localhost:8080')).toBe(true);
    expect(isLoopbackUrl('http://[::1]:8080')).toBe(true);
    expect(isLoopbackUrl('http://192.168.1.20')).toBe(false);
    expect(isLoopbackUrl('not a url')).toBe(false);
  });
});
