import { OIDC_START, oidcStartHref, parseOIDCReturn, takeOIDCReturn } from './oidc-return';

const KEY = 'aZ09-_aZ09-_aZ09';

describe('parseOIDCReturn', () => {
  it("knows a single sign-on's return, with its flow's key or from a server before it", () => {
    expect(parseOIDCReturn(`#oidc=done&flow=${KEY}`)).toEqual({ flow: KEY });
    expect(parseOIDCReturn('#oidc=done')).toEqual({});
  });

  it('ignores anything else, a token in the address included', () => {
    for (const hash of [
      '',
      '#',
      '#oidc=',
      '#oidc=done&x=1',
      '#oidc=done&flow=',
      '#oidc=done&flow=short',
      `#oidc=done&flow=${KEY}x`,
      `#oidc=done&flow=${KEY}&x=1`,
      '#oidc=done&flow=aZ09.aZ09/aZ09+aZ',
      '#oidc_token=abc',
      '#other=done',
    ]) {
      expect(parseOIDCReturn(hash)).toBeNull();
    }
  });
});

describe('takeOIDCReturn', () => {
  it('reports the return and replaces the history entry without it', () => {
    const replaceState = vi.fn();
    const state = { key: 'k' };
    const done = takeOIDCReturn(
      { hash: `#oidc=done&flow=${KEY}`, pathname: '/admin/', search: '?a=1' },
      { replaceState, state },
    );
    expect(done).toEqual({ flow: KEY });
    expect(replaceState).toHaveBeenCalledWith(state, '', '/admin/?a=1');
  });

  it('leaves the address alone without one', () => {
    const replaceState = vi.fn();
    expect(
      takeOIDCReturn(
        { hash: '#x', pathname: '/admin/', search: '' },
        { replaceState, state: null },
      ),
    ).toBeNull();
    expect(replaceState).not.toHaveBeenCalled();
  });
});

describe('oidcStartHref', () => {
  it('carries the console page asked for, so the sign-in lands back on it', () => {
    expect(oidcStartHref({ pathname: '/admin/people/user/3', search: '?tab=devices' })).toBe(
      `${OIDC_START}&next=${encodeURIComponent('/admin/people/user/3?tab=devices')}`,
    );
  });

  it('goes without for the start page or anything outside the console', () => {
    for (const pathname of ['/admin/', '/admin', '/', '/connect']) {
      expect(oidcStartHref({ pathname, search: '' })).toBe(OIDC_START);
    }
  });
});
