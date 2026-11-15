import { oidcTokenFrom, takeOIDCToken } from './oidc-return';

describe('oidcTokenFrom', () => {
  it('reads the token a single sign-on lands on', () => {
    expect(oidcTokenFrom('#oidc_token=abc_DEF-123')).toBe('abc_DEF-123');
  });

  it('ignores anything else', () => {
    for (const hash of [
      '',
      '#',
      '#oidc_token=',
      '#other=abc',
      '#oidc_token=a b',
      '#oidc_token=a&x=1',
    ]) {
      expect(oidcTokenFrom(hash)).toBeNull();
    }
  });
});

describe('takeOIDCToken', () => {
  it('returns the token and replaces the history entry without it', () => {
    const replaceState = vi.fn();
    const state = { key: 'k' };
    const token = takeOIDCToken(
      { hash: '#oidc_token=tok', pathname: '/admin/', search: '?a=1' },
      { replaceState, state },
    );
    expect(token).toBe('tok');
    expect(replaceState).toHaveBeenCalledWith(state, '', '/admin/?a=1');
  });

  it('leaves the address alone without one', () => {
    const replaceState = vi.fn();
    expect(
      takeOIDCToken({ hash: '#x', pathname: '/admin/', search: '' }, { replaceState, state: null }),
    ).toBeNull();
    expect(replaceState).not.toHaveBeenCalled();
  });
});
