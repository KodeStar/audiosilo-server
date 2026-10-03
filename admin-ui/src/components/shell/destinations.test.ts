import { destinationFor } from './destinations';

describe('destinationFor', () => {
  it('maps a router path to its destination', () => {
    expect(destinationFor('/library')?.key).toBe('library');
    expect(destinationFor('/library/authors')?.key).toBe('library');
    expect(destinationFor('/server/logs')?.key).toBe('server');
  });

  it('leaves home and look-alikes alone', () => {
    expect(destinationFor('/')).toBeUndefined();
    expect(destinationFor('/libraryx')).toBeUndefined();
  });
});
