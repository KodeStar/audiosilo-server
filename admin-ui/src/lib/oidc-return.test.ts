import { isOIDCReturn, takeOIDCReturn } from './oidc-return';

describe('isOIDCReturn', () => {
  it("knows a single sign-on's return", () => {
    expect(isOIDCReturn('#oidc=done')).toBe(true);
  });

  it('ignores anything else, a token in the address included', () => {
    for (const hash of ['', '#', '#oidc=', '#oidc=done&x=1', '#oidc_token=abc', '#other=done']) {
      expect(isOIDCReturn(hash)).toBe(false);
    }
  });
});

describe('takeOIDCReturn', () => {
  it('reports the return and replaces the history entry without it', () => {
    const replaceState = vi.fn();
    const state = { key: 'k' };
    const done = takeOIDCReturn(
      { hash: '#oidc=done', pathname: '/admin/', search: '?a=1' },
      { replaceState, state },
    );
    expect(done).toBe(true);
    expect(replaceState).toHaveBeenCalledWith(state, '', '/admin/?a=1');
  });

  it('leaves the address alone without one', () => {
    const replaceState = vi.fn();
    expect(
      takeOIDCReturn(
        { hash: '#x', pathname: '/admin/', search: '' },
        { replaceState, state: null },
      ),
    ).toBe(false);
    expect(replaceState).not.toHaveBeenCalled();
  });
});
