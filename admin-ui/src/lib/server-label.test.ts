import { serverLabel } from './server-label';

describe('serverLabel', () => {
  it('uses a name the admin set', () => {
    expect(serverLabel('Tank', 'localhost:8877')).toBe('Tank');
    expect(serverLabel('  Tank  ', 'localhost:8877')).toBe('Tank');
  });

  it('falls back to the host for no name or the default one', () => {
    expect(serverLabel(undefined, 'localhost:8877')).toBe('localhost:8877');
    expect(serverLabel('', 'localhost:8877')).toBe('localhost:8877');
    expect(serverLabel('  ', 'localhost:8877')).toBe('localhost:8877');
    expect(serverLabel('AudioSilo', 'localhost:8877')).toBe('localhost:8877');
  });
});
