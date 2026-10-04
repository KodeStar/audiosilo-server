import { afterEach, describe, expect, it } from 'vitest';
import { browserId } from './browser-id';

describe('browserId', () => {
  afterEach(() => localStorage.clear());

  it('makes a random id once and keeps it', () => {
    const id = browserId();
    expect(id).toMatch(/^[0-9a-f]{32}$/);
    expect(browserId()).toBe(id);
    localStorage.clear();
    expect(browserId()).not.toBe(id);
  });

  it('replaces a stored value the server would ignore', () => {
    localStorage.setItem('audiosilo_browser_id', 'short');
    expect(browserId()).toMatch(/^[0-9a-f]{32}$/);
  });
});
